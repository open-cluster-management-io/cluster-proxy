package selfsigned

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	TLSCACert = "ca.crt"
	TLSCAKey  = "ca.key"
)

func DumpCASecret(c kubernetes.Interface, namespace, name string, caCertData, caKeyData []byte) (bool, error) {
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
	_, err := c.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return true, nil
	}
	return false, err
}
