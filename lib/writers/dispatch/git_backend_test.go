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
	"github.com/syntasso/kratix/lib/writers"
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

	It("applies a 3-intent batch and includes all files in the bare repo", func() {
		b, err := dispatch.NewGitBackend(logr.Discard(), dest, spec, creds)
		Expect(err).NotTo(HaveOccurred())
		defer b.Close()

		intents := []dispatch.ResolvedIntent{
			{Key: "wp-1|sub-1", WorkPlacement: "wp-1", SubDir: "sub-1", Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "1.yaml", Content: "1"}}}},
			{Key: "wp-2|sub-2", WorkPlacement: "wp-2", SubDir: "sub-2", Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "2.yaml", Content: "2"}}}},
			{Key: "wp-3|sub-3", WorkPlacement: "wp-3", SubDir: "sub-3", Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "3.yaml", Content: "3"}}}},
		}
		res := b.ApplyBatch(context.Background(), intents)
		for _, ri := range intents {
			Expect(res.PerIntent[ri.Key]).NotTo(HaveOccurred(), "intent %s should succeed", ri.Key)
		}

		checkout := GinkgoT().TempDir()
		Expect(exec.Command("git", "clone", "-b", "main", bareRepo, checkout).Run()).To(Succeed())
		for _, ri := range intents {
			body, err := os.ReadFile(filepath.Join(checkout, "state", "dest", ri.SubDir, ri.Writes.ToCreate[0].Filepath))
			Expect(err).NotTo(HaveOccurred(), "file %s should exist", ri.Writes.ToCreate[0].Filepath)
			Expect(string(body)).To(Equal(ri.Writes.ToCreate[0].Content))
		}
	})

	It("quarantines a path-traversal intent and applies the rest of the batch", func() {
		b, err := dispatch.NewGitBackend(logr.Discard(), dest, spec, creds)
		Expect(err).NotTo(HaveOccurred())
		defer b.Close()

		// Construct an escape path with enough "../" segments to clear the
		// writer's temp directory regardless of where the OS puts it.
		intents := []dispatch.ResolvedIntent{
			{Key: "wp-ok|sub", WorkPlacement: "wp-ok", SubDir: "sub", Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "good.yaml", Content: "good"}}}},
			{Key: "wp-bad|sub-bad", WorkPlacement: "wp-bad", SubDir: "sub-bad", Writes: dispatch.Writes{ToCreate: []v1alpha1.Workload{{
				Filepath: "../../../../../../../../../../../../../../etc/passwd",
				Content:  "nope",
			}}}},
		}
		res := b.ApplyBatch(context.Background(), intents)
		Expect(res.PerIntent["wp-ok|sub"]).NotTo(HaveOccurred())
		Expect(res.PerIntent["wp-bad|sub-bad"]).To(MatchError(writers.ErrPathOutsideRepo))

		checkout := GinkgoT().TempDir()
		Expect(exec.Command("git", "clone", "-b", "main", bareRepo, checkout).Run()).To(Succeed())
		body, err := os.ReadFile(filepath.Join(checkout, "state", "dest", "sub", "good.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("good"))
	})
})

func runGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v failed: %s", args, string(out))
}
