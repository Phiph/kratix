package dispatch_test

import (
	"context"
	"os"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers/dispatch"
)

var _ = Describe("S3Backend integration", func() {
	var (
		endpoint, bucket, ak, sk string
		spec                     v1alpha1.BucketStateStoreSpec
		creds                    map[string][]byte
		dest                     dispatch.DestinationKey
	)

	BeforeEach(func() {
		endpoint = os.Getenv("KRATIX_S3_TEST_ENDPOINT")
		bucket = os.Getenv("KRATIX_S3_TEST_BUCKET")
		ak = os.Getenv("KRATIX_S3_TEST_ACCESS_KEY")
		sk = os.Getenv("KRATIX_S3_TEST_SECRET_KEY")
		if endpoint == "" || bucket == "" {
			Skip("S3Backend tests require KRATIX_S3_TEST_ENDPOINT and KRATIX_S3_TEST_BUCKET")
		}
		spec = v1alpha1.BucketStateStoreSpec{
			StateStoreCoreFields: v1alpha1.StateStoreCoreFields{
				Path:      "p",
				SecretRef: &corev1.SecretReference{Namespace: "default", Name: "s"},
			},
			AuthMethod: "accessKey",
			Endpoint:   endpoint,
			BucketName: bucket,
			Insecure:   true,
		}
		creds = map[string][]byte{
			"accessKeyID":     []byte(ak),
			"secretAccessKey": []byte(sk),
		}
		dest = dispatch.DestinationKey{
			StateStoreKind: "BucketStateStore",
			StateStoreName: "b",
			Path:           "p",
		}
	})

	It("applies a single-intent batch and reads back the written object", func() {
		b, err := dispatch.NewS3Backend(logr.Discard(), dest, spec, creds)
		Expect(err).NotTo(HaveOccurred())
		defer b.Close()

		res := b.ApplyBatch(context.Background(), []dispatch.ResolvedIntent{{
			Key: "wp|", WorkPlacement: "wp", SubDir: "",
			Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "test.yaml", Content: "hello"}}},
		}})
		Expect(res.PerIntent["wp|"]).NotTo(HaveOccurred())

		out, err := b.Read(context.Background(), []string{"test.yaml"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(HaveKeyWithValue("test.yaml", []byte("hello")))
	})
})
