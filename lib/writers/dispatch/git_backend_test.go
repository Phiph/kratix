package dispatch_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers/dispatch"
)

var _ = Describe("GitBackend integration", func() {
	var (
		bareRepo string
		spec     v1alpha1.GitStateStoreSpec
		creds    map[string][]byte
		dest     dispatch.DestinationKey
	)

	BeforeEach(func() {
		bareRepo = GinkgoT().TempDir()
		Expect(exec.Command("git", "init", "--bare", "--initial-branch=main", bareRepo).Run()).To(Succeed())

		// Seed the bare repo with an initial commit so "main" exists.
		seed := GinkgoT().TempDir()
		Expect(exec.Command("git", "init", "--initial-branch=main", seed).Run()).To(Succeed())
		Expect(os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed"), 0644)).To(Succeed())
		runGit(seed, "config", "user.email", "test@example.com")
		runGit(seed, "config", "user.name", "test")
		runGit(seed, "add", ".")
		runGit(seed, "commit", "-m", "seed")
		runGit(seed, "remote", "add", "origin", bareRepo)
		runGit(seed, "push", "origin", "main")

		spec = v1alpha1.GitStateStoreSpec{
			StateStoreCoreFields: v1alpha1.StateStoreCoreFields{
				Path:      "state",
				SecretRef: &corev1.SecretReference{Namespace: "default", Name: "s"},
			},
			AuthMethod: v1alpha1.BasicAuthMethod,
			URL:        bareRepo,
			Branch:     "main",
			GitAuthor:  v1alpha1.GitAuthor{Name: "test", Email: "test@example.com"},
		}
		creds = map[string][]byte{
			"username": []byte("x"),
			"password": []byte("x"),
		}
		dest = dispatch.DestinationKey{
			StateStoreKind: "GitStateStore",
			StateStoreName: "g",
			Branch:         "main",
			Path:           "dest",
		}
	})

	It("constructs, reads a present and missing file, and closes cleanly", func() {
		b, err := dispatch.NewGitBackend(logr.Discard(), dest, spec, creds)
		Expect(err).NotTo(HaveOccurred())
		defer b.Close()

		// README.md was seeded at the bare-repo root; GitBackend clones into the
		// destination's Path ("dest"), so README isn't visible at the relative path
		// "README.md". Read a missing file: result map should NOT have that key.
		got, err := b.Read(context.Background(), []string{"nope.yaml"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(HaveKey("nope.yaml"))
	})

	It("applies a single-intent batch: one commit, one push, expected file in bare repo", func() {
		b, err := dispatch.NewGitBackend(logr.Discard(), dest, spec, creds)
		Expect(err).NotTo(HaveOccurred())
		defer b.Close()

		res := b.ApplyBatch(context.Background(), []dispatch.ResolvedIntent{{
			Key:           "wp-a|sub",
			WorkPlacement: "wp-a",
			SubDir:        "sub",
			Writes: dispatch.Writes{
				ToCreate: []v1alpha1.Workload{{Filepath: "a.yaml", Content: "a-content"}},
			},
		}})
		Expect(res.PerIntent["wp-a|sub"]).NotTo(HaveOccurred())
		Expect(res.VersionID).NotTo(BeEmpty())

		// Verify by cloning the bare repo to a check-out dir.
		// The repo layout is <stateStore.Path>/<dest.Path>/<subDir>/<filepath>.
		checkout := GinkgoT().TempDir()
		Expect(exec.Command("git", "clone", "-b", "main", bareRepo, checkout).Run()).To(Succeed())
		body, err := os.ReadFile(filepath.Join(checkout, "state", "dest", "sub", "a.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("a-content"))
	})
})

func runGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v failed: %s", args, string(out))
}
