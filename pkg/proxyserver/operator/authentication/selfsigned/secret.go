package selfsigned

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

const (
	TLSCACert = "ca.crt"
	TLSCAKey  = "ca.key"
)

// NewOwnerReferenceFromConfig creates an OwnerReference from a ManagedProxyConfiguration.
func NewOwnerReferenceFromConfig(config *proxyv1alpha1.ManagedProxyConfiguration) *metav1.OwnerReference {
	return &metav1.OwnerReference{
		APIVersion:         proxyv1alpha1.GroupVersion.String(),
		Kind:               "ManagedProxyConfiguration",
		Name:               config.Name,
		UID:                config.UID,
		BlockOwnerDeletion: ptr.To(true),
	}
}

func DumpCASecret(c kubernetes.Interface, namespace, name string, caCertData, caKeyData []byte, ownerRef *metav1.OwnerReference) (bool, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TLSCACert: caCertData,
			TLSCAKey:  caKeyData,
		},
	}
	if ownerRef != nil {
		secret.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	}
	_, err := c.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return true, nil
	}
	return false, err
}
