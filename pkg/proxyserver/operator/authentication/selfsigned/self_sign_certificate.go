package selfsigned

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"open-cluster-management.io/cluster-proxy/pkg/common"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/cert"
)

var (
	rsaKeySize = 2048 // a decent number, as of 2019
	bigOne     = big.NewInt(1)
)

type CertRotation interface {
	EnsureTargetCertKeyPair(signingCertKeyPair *openshiftcrypto.CA, caBundleCerts []*x509.Certificate, fns ...openshiftcrypto.CertificateExtensionFunc) error
}

type SelfSigner interface {
	Sign(cfg cert.Config, expiry time.Duration) (CertPair, error)
	CAData() []byte
	GetSigner() crypto.Signer
	CA() *openshiftcrypto.CA
}

var _ SelfSigner = &selfSigner{}

type selfSigner struct {
	caCert     *x509.Certificate
	caKey      crypto.Signer
	nextSerial *big.Int
}

func NewSelfSignerFromSecretOrGenerate(c kubernetes.Interface, secretNamespace, secretName string, ownerRef *metav1.OwnerReference) (SelfSigner, error) {
	shouldGenerate := false
	caSecret, err := c.CoreV1().Secrets(secretNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, errors.Wrapf(err, "failed to read ca from secret %v/%v", secretNamespace, secretName)
		}
		shouldGenerate = true
	}
	if !shouldGenerate {
		// Update owner reference if missing and ownerRef is provided
		if ownerRef != nil && !hasOwnerReference(caSecret.OwnerReferences, ownerRef) {
			caSecret.OwnerReferences = []metav1.OwnerReference{*ownerRef}
			_, err = c.CoreV1().Secrets(secretNamespace).Update(context.TODO(), caSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrapf(err, "failed to update owner reference for secret %v/%v", secretNamespace, secretName)
			}
		}
		return NewSelfSignerWithCAData(caSecret.Data[TLSCACert], caSecret.Data[TLSCAKey])
	}
	generatedSigner, err := NewGeneratedSelfSigner()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate new self-signer")
	}

	rawKeyData, err := x509.MarshalPKCS8PrivateKey(generatedSigner.GetSigner())
	if err != nil {
		return nil, err
	}
	caKeyData := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: rawKeyData,
	})
	isAlreadyExists, err := DumpCASecret(c,
		secretNamespace, secretName,
		generatedSigner.CAData(), caKeyData, ownerRef)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dump generated ca secret %v/%v", secretNamespace, secretName)
	}
	if isAlreadyExists {
		return NewSelfSignerFromSecretOrGenerate(c, secretNamespace, secretName, ownerRef)
	}
	return generatedSigner, nil
}

func hasOwnerReference(refs []metav1.OwnerReference, target *metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.UID == target.UID {
			return true
		}
	}
	return false
}

func NewGeneratedSelfSigner() (SelfSigner, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, err
	}
	caCert, err := cert.NewSelfSignedCACert(cert.Config{
		CommonName: common.AddonFullName,
	}, privateKey)
	if err != nil {
		return nil, err
	}
	return NewSelfSignerWithCA(caCert, privateKey, big.NewInt(1))
}

func NewSelfSignerWithCAData(caCertData, caKeyData []byte) (SelfSigner, error) {
	certBlock, _ := pem.Decode(caCertData)
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse ca certificate")
	}
	keyBlock, _ := pem.Decode(caKeyData)
	caKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse ca key")
	}
	next := big.NewInt(0)
	next.Add(caCert.SerialNumber, big.NewInt(1))
	return NewSelfSignerWithCA(caCert, caKey.(*rsa.PrivateKey), next)
}

func NewSelfSignerWithCA(caCert *x509.Certificate, caKey *rsa.PrivateKey, nextSerial *big.Int) (SelfSigner, error) {
	return &selfSigner{
		caCert:     caCert,
		caKey:      caKey,
		nextSerial: nextSerial,
	}, nil
}

func (s selfSigner) Sign(cfg cert.Config, expiry time.Duration) (CertPair, error) {
	now := time.Now()

	key, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return CertPair{}, fmt.Errorf("unable to create private key: %v", err)
	}

	serial := new(big.Int).Set(s.nextSerial)
	s.nextSerial.Add(s.nextSerial, bigOne)

	template := x509.Certificate{
		Subject:      pkix.Name{CommonName: cfg.CommonName, Organization: cfg.Organization},
		DNSNames:     cfg.AltNames.DNSNames,
		IPAddresses:  cfg.AltNames.IPs,
		SerialNumber: serial,
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  cfg.Usages,
		NotBefore:    now.UTC(),
		NotAfter:     now.Add(expiry).UTC(),
	}

	certRaw, err := x509.CreateCertificate(rand.Reader, &template, s.caCert, key.Public(), s.caKey)
	if err != nil {
		return CertPair{}, fmt.Errorf("unable to create certificate: %v", err)
	}

	certificate, err := x509.ParseCertificate(certRaw)
	if err != nil {
		return CertPair{}, fmt.Errorf("generated invalid certificate, could not parse: %v", err)
	}

	return CertPair{
		Key:  key,
		Cert: certificate,
	}, nil
}

func (s selfSigner) CAData() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: s.caCert.Raw,
	})
}

func (s selfSigner) GetSigner() crypto.Signer {
	return s.caKey
}

func (s selfSigner) CA() *openshiftcrypto.CA {
	return &openshiftcrypto.CA{
		Config: &openshiftcrypto.TLSCertificateConfig{
			Certs: []*x509.Certificate{s.caCert},
			Key:   s.caKey,
		},
		SerialGenerator: &openshiftcrypto.RandomSerialGenerator{},
	}
}

type CertPair struct {
	Key  crypto.Signer
	Cert *x509.Certificate
}

func (k CertPair) CertBytes() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: k.Cert.Raw,
	})
}

func (k CertPair) AsBytes() (cert []byte, key []byte, err error) {
	cert = k.CertBytes()

	rawKeyData, err := x509.MarshalPKCS8PrivateKey(k.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to encode private key: %v", err)
	}

	key = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: rawKeyData,
	})

	return cert, key, nil
}
