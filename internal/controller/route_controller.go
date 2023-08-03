/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	networkingv1alpha1 "github.com/lake-of-dreams/routes-operator/api/v1alpha1"
	k8net "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	labelKey   = "verrazzano.io/managed-by"
	labelValue = "routes-operator"
)

// RouteReconciler reconciles a Route object
type RouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=networking.verrazzano.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.verrazzano.io,resources=routes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.verrazzano.io,resources=routes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Route object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *RouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	route := &networkingv1alpha1.Route{}
	err := r.Get(ctx, req.NamespacedName, route)

	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileStarted"})
	r.Status().Update(ctx, route)

	if route.GetDeletionTimestamp() != nil {
		return r.handleDelete(ctx, route, logger)
	}

	return r.mutateRoute(ctx, route, logger)

}

func (r *RouteReconciler) mutateRoute(ctx context.Context, route *networkingv1alpha1.Route, logger logr.Logger) (ctrl.Result, error) {
	if route.Spec.Type == networkingv1alpha1.TypeIngress {
		ingresses := &k8net.IngressList{}
		err := r.List(ctx, ingresses, &client.MatchingLabels{labelKey: labelValue}, client.InNamespace(route.GetNamespace()))
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		if len(ingresses.Items) > 0 {
			for _, ingress := range ingresses.Items {
				for _, ingressRule := range ingress.Spec.Rules {
					if ingressRule.HTTP != nil {
						for _, ingressPath := range ingressRule.HTTP.Paths {
							if ingressPath.Backend.Service != nil &&
								ingressPath.Backend.Service.Name == route.Spec.Destination.Service &&
								ingressPath.Backend.Service.Port.Number == route.Spec.Destination.Port {
								if route.Spec.Destination.Path == ingressPath.Path {
									logger.V(10).Info("ingress already exists for the Route, update host in status")
									return r.updateHostInStatus(ctx, route, ingressRule.Host)
								}
							}
						}
					}
				}
			}
		}

		const externalDNSKey = "external-dns.alpha.kubernetes.io/target"

		// Extract the domain name from the Verrazzano vzIngress
		vzIngress := k8net.Ingress{}
		err = r.Get(ctx, types.NamespacedName{Name: ingressVerrazzano, Namespace: namespaceVerrazzanoSystem}, &vzIngress)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		externalDNSAnno, ok := vzIngress.Annotations[externalDNSKey]
		if !ok || len(externalDNSAnno) == 0 {
			return ctrl.Result{Requeue: false}, fmt.Errorf("annotation %s missing from verrazzano ingress, unable to generate DNS name", externalDNSKey)
		}

		domain := externalDNSAnno[len(ingressVerrazzano)+1:]
		hostName := fmt.Sprintf("%s.%s.%s", route.GetName(), route.GetNamespace(), domain)
		pathType := k8net.PathTypeImplementationSpecific
		trueType := true
		// create the ingress in the same namespace as Auth Proxy, note that we cannot use authproxy.ComponentNamespace here because it creates an import cycle
		ingress := k8net.Ingress{}
		_, err = controllerruntime.CreateOrUpdate(context.TODO(), r.Client, &ingress, func() error {
			ingress = k8net.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-%s", route.GetName(), "ingress"),
					Namespace: route.GetNamespace(),
					Annotations: map[string]string{
						"kubernetes.io/tls-acme":         vzIngress.Annotations["kubernetes.io/tls-acme"],
						"cert-manager.io/cluster-issuer": vzIngress.Annotations["cert-manager.io/cluster-issuer"],
						"cert-manager.io/common-name":    hostName,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         route.APIVersion,
							Kind:               route.Kind,
							Name:               route.Name,
							BlockOwnerDeletion: &trueType,
						},
					},
				},
				Spec: k8net.IngressSpec{
					IngressClassName: vzIngress.Spec.IngressClassName,
					TLS: []k8net.IngressTLS{
						{
							Hosts:      []string{hostName},
							SecretName: fmt.Sprintf("%s-%s", route.GetName(), "ingress-tls-secret"),
						},
					},
					Rules: []k8net.IngressRule{
						{
							Host: hostName,
							IngressRuleValue: k8net.IngressRuleValue{
								HTTP: &k8net.HTTPIngressRuleValue{
									Paths: []k8net.HTTPIngressPath{
										{
											Path:     route.Spec.Destination.Path,
											PathType: &pathType,
											Backend: k8net.IngressBackend{
												Service: &k8net.IngressServiceBackend{
													Name: route.Spec.Destination.Service,
													Port: k8net.ServiceBackendPort{
														Number: route.Spec.Destination.Port,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}

			return nil
		})

		if err != nil {
			route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileFinished"})
			return ctrl.Result{Requeue: true}, err
		}
		route.Status.Host = ingress.Spec.TLS[0].Hosts[0]
	}
	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileFinished"})
	return ctrl.Result{Requeue: false}, nil
}

func (r *RouteReconciler) updateHostInStatus(ctx context.Context, route *networkingv1alpha1.Route, hostName string) (ctrl.Result, error) {
	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "Host Updated"})
	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileFinished"})
	route.Status.Host = hostName
	return ctrl.Result{}, r.Status().Update(ctx, route)
}

func (r *RouteReconciler) handleDelete(ctx context.Context, route *networkingv1alpha1.Route, logger logr.Logger) (ctrl.Result, error) {
	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "Deleting"})
	r.Status().Update(ctx, route)
	ingresses := &k8net.IngressList{}
	err := r.List(ctx, ingresses, &client.MatchingLabels{labelKey: labelValue}, client.InNamespace(route.GetNamespace()))
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	if len(ingresses.Items) > 0 {
		for _, ingress := range ingresses.Items {
			for _, owner := range ingress.OwnerReferences {
				if owner.APIVersion == route.APIVersion && owner.Kind == route.Kind && owner.Name == route.Name {
					if ingress.GetDeletionTimestamp() == nil {
						r.Delete(ctx, &ingress)
					}
				}
			}
		}
	} else {
		route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileFinished"})
		r.Status().Update(ctx, route)
		return ctrl.Result{Requeue: false}, nil
	}
	route.Status.Conditions = append(route.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Type: "ReconcileFinished"})
	r.Status().Update(ctx, route)
	return ctrl.Result{Requeue: true}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.Route{}).
		Complete(r)
}
