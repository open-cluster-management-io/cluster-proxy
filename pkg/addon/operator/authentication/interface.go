package authentication

import "k8s.io/client-go/util/cert"

type CertificateSigner interface {
	Sign(certConfig cert.Config) (certData, keyData []byte, err error)
}

type CertificateSignerGetter interface {
	Get() (CertificateSigner, error)
}
