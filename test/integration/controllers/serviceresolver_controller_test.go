package controllers

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("ServiceResolver Reconciler", func() {
	const (
		timeout  = time.Second * 30
		interval = time.Second * 1
	)

	Context("When service resolver is not legal", func() {
		It("Should return condition equals False,  and reason is ManagedProxyServiceResolverNotLegal", func() {
			serviceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sr-test-illegal",
				},
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type:              proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: nil,
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
						ServiceRef: &proxyv1alpha1.ServiceRef{
							Name:      "hello-world",
							Namespace: "default",
						},
					},
				},
			}

			err := ctrlClient.Create(ctx, serviceResolver)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("no condition found")
				}
				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionFalse {
							return fmt.Errorf("condition is not false, %v", currentServiceResolver.Status.Conditions)
						}
					}
				}
				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})
	})

	Context("When service resolver created, and clusterset is found", func() {
		It("Should return confition equals True", func() {
			serviceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sr-test",
				},
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "sr-test",
						},
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
						ServiceRef: &proxyv1alpha1.ServiceRef{
							Name:      "hello-world",
							Namespace: "default",
						},
					},
				},
			}
			clusterset := &clusterv1beta1.ManagedClusterSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sr-test",
				},
			}

			err := ctrlClient.Create(ctx, serviceResolver)
			Expect(err).ToNot(HaveOccurred())

			err = ctrlClient.Create(ctx, clusterset)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionTrue {
							return fmt.Errorf("condition is not true, %v", currentServiceResolver.Status.Conditions)
						}
					}
				}

				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})
	})

	Context("When service resolver created, but the clusterset is not found", func() {
		It("Should return confition equals False, and reason is ManagedClusterSetNotExists", func() {
			serviceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sr-test2",
				},
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "sr-test2", // clusterset not exists
						},
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
						ServiceRef: &proxyv1alpha1.ServiceRef{
							Name:      "hello-world",
							Namespace: "default",
						},
					},
				},
			}

			err := ctrlClient.Create(ctx, serviceResolver)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionFalse {
							return fmt.Errorf("condition is not false, %v", currentServiceResolver.Status.Conditions)
						}
						if condition.Reason != "ManagedClusterSetNotExisted" {
							return fmt.Errorf("reason is not ManagedClusterSetNotExisted")
						}
					}
				}

				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})
	})

	Context("When the cluserset is deleting", func() {
		serviceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{
			ObjectMeta: metav1.ObjectMeta{
				Name: "sr-test3",
			},
			Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
				ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
					Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
					ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
						Name: "sr-test3",
					},
				},
				ServiceSelector: proxyv1alpha1.ServiceSelector{
					Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
					ServiceRef: &proxyv1alpha1.ServiceRef{
						Name:      "hello-world",
						Namespace: "default",
					},
				},
			},
		}
		clusterset := &clusterv1beta1.ManagedClusterSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "sr-test3",
				Finalizers: []string{
					"block",
				},
			},
		}

		BeforeEach(func() {
			err := ctrlClient.Create(ctx, serviceResolver)
			Expect(err).ToNot(HaveOccurred())

			err = ctrlClient.Create(ctx, clusterset)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionTrue {
							return fmt.Errorf("condition is not true, %v", currentServiceResolver.Status.Conditions)
						}
					}
				}
				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})

		It("Should return confition equals False, and reason is ManagedClusterSetDeleting", func() {
			Eventually(func() error {
				currentClusterSet := &clusterv1beta1.ManagedClusterSet{}
				err := ctrlClient.Get(ctx, client.ObjectKey{Name: clusterset.Name}, currentClusterSet)
				if err != nil {
					return err
				}
				return ctrlClient.Delete(ctx, currentClusterSet)
			}, 3*timeout, 3*interval).Should(Succeed())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err := ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}

				currentClusterSet := &clusterv1beta1.ManagedClusterSet{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: clusterset.Name}, currentClusterSet)
				if err != nil {
					return err
				}

				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionFalse {
							return fmt.Errorf("condition is not false, %v, set deletionstamp:%v", currentServiceResolver.Status.Conditions, currentClusterSet.DeletionTimestamp)
						}
						if condition.Reason != "ManagedClusterSetDeleting" {
							return fmt.Errorf("reason is not ManagedClusterSetDeleting")
						}
					}
				}
				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})
	})

	Context("When change the clusterset of the service resolver", func() {
		serviceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{
			ObjectMeta: metav1.ObjectMeta{
				Name: "sr-test4",
			},
			Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
				ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
					Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
					ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
						Name: "sr-test4",
					},
				},
				ServiceSelector: proxyv1alpha1.ServiceSelector{
					Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
					ServiceRef: &proxyv1alpha1.ServiceRef{
						Name:      "hello-world",
						Namespace: "default",
					},
				},
			},
		}
		clusterset := &clusterv1beta1.ManagedClusterSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "sr-test4",
			},
		}

		BeforeEach(func() {
			err := ctrlClient.Create(ctx, serviceResolver)
			Expect(err).ToNot(HaveOccurred())

			err = ctrlClient.Create(ctx, clusterset)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err = ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}

				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionTrue {
							return fmt.Errorf("condition is not true, %v", currentServiceResolver.Status.Conditions)
						}
					}
				}
				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})

		It("Should return confition equals False, and reason is ManagedClusterSetNotExisted", func() {
			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err := ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				currentServiceResolver.Spec.ManagedClusterSelector.ManagedClusterSet.Name = "sr-test5"
				return ctrlClient.Update(ctx, currentServiceResolver)
			}, 3*timeout, 3*interval).Should(Succeed())

			Eventually(func() error {
				currentServiceResolver := &proxyv1alpha1.ManagedProxyServiceResolver{}
				err := ctrlClient.Get(ctx, client.ObjectKey{Name: serviceResolver.Name}, currentServiceResolver)
				if err != nil {
					return err
				}
				if len(currentServiceResolver.Status.Conditions) == 0 {
					return fmt.Errorf("not enough conditions found")
				}

				for _, condition := range currentServiceResolver.Status.Conditions {
					if condition.Type == proxyv1alpha1.ConditionTypeServiceResolverAvaliable {
						if condition.Status != metav1.ConditionFalse {
							return fmt.Errorf("condition is not false, %v", currentServiceResolver.Status.Conditions)
						}
						if condition.Reason != "ManagedClusterSetNotExisted" {
							return fmt.Errorf("reason is not ManagedClusterSetNotExisted")
						}
					}
				}
				return nil
			}, 3*timeout, 3*interval).Should(Succeed())
		})
	})
})
