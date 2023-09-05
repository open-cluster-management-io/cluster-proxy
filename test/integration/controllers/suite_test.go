/*
Copyright 2021.

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

package controllers

import (
	"context"
	"crypto"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/cert"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/controllers"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	//+kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var ctrlClient client.Client
var kubeClient kubernetes.Interface
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

type SelfSigner interface {
	Sign(cfg cert.Config, expiry time.Duration) (selfsigned.CertPair, error)
	CAData() []byte
	GetSigner() crypto.Signer
	CA() *openshiftcrypto.CA
}

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "hack", "crd", "addon"),
			filepath.Join("..", "..", "..", "hack", "crd", "bases"),
			filepath.Join("..", "..", "..", "hack", "crd", "cluster"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme := runtime.NewScheme()
	err = clientgoscheme.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = addonv1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = proxyv1alpha1.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	err = clusterv1beta2.AddToScheme(scheme)
	Expect(err).NotTo(HaveOccurred())

	kubeClient, err = kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	kubeInformer := informers.NewSharedInformerFactory(kubeClient, 30*time.Minute)

	ctx, cancel = context.WithCancel(context.TODO())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	ctrlClient = mgr.GetClient()

	selfSigner, err := selfsigned.NewSelfSignerFromSecretOrGenerate(kubeClient, "default", "test-ca")
	Expect(err).NotTo(HaveOccurred())

	err = controllers.RegisterClusterManagementAddonReconciler(mgr, selfSigner, kubeClient, kubeInformer.Core().V1().Secrets(), true)
	Expect(err).NotTo(HaveOccurred())

	err = controllers.RegisterServiceResolverReconciler(mgr)
	Expect(err).NotTo(HaveOccurred())

	By("start manager")
	go kubeInformer.Start(ctx.Done())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Printf("failed to start manager, %v\n", err)
			os.Exit(1)
		}
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
