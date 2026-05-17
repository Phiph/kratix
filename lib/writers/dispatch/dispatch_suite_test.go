package dispatch_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDispatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Dispatch Suite")
}

var _ = BeforeSuite(func() {
	Expect(os.Setenv("KRATIX_GIT_ALLOW_FILE_URL_FOR_TEST", "1")).To(Succeed())
})

var _ = AfterSuite(func() {
	Expect(os.Unsetenv("KRATIX_GIT_ALLOW_FILE_URL_FOR_TEST")).To(Succeed())
})
