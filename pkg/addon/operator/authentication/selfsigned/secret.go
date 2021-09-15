package selfsigned

import (
	"context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	TLSCACert = "ca.crt"
	TLSCAKey  = "ca.key"
)

func DumpSecret(c client.Client, namespace, name string, caData, certData, keyData []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certData,
			corev1.TLSPrivateKeyKey: keyData,
			TLSCACert:               caData,
		},
	}
	return c.Create(context.TODO(), secret)
}

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
