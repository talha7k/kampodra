package cloud

import (
	"context"
	"strings"
	"testing"
)

// The image-import family is OCI-only BY DESIGN (owner-approved, D2):
// `gcloud compute images import` requires enabling the Cloud Build API — a
// billing-bearing project-level mutation kampodra must never trigger
// implicitly — and the import itself is a 10-25min async pipeline. Every
// neutral op therefore fails closed on a gcp profile with the ONE refusal
// text (the same const the command surface's entry guard renders), zero
// execs of either CLI: the wrong-CLI exec is the failure slice A closed,
// and an improvised gcloud argv must never exist either.

// wantImageImportRefusal pins the contract LITERAL — the const and both
// emission sites (entry guard + op defaults) must render exactly this.
const wantImageImportRefusal = "image-import is OCI-only today — gcloud compute images import requires the Cloud Build API; run it directly or use an oci profile"

// imageOpsAll maps every neutral image op to a minimal invocation — the
// shared driver for the gcp fail-closed pin.
func imageOpsAll() map[string]func(context.Context, CloudAuth) error {
	comp := "ocid1.compartment.test"
	return map[string]func(context.Context, CloudAuth) error{
		"ImageStage": func(ctx context.Context, a CloudAuth) error {
			return ImageStage(ctx, a, "staging-bkt", "app.qcow2", "app-1.qcow2")
		},
		"ImageImport": func(ctx context.Context, a CloudAuth) error {
			_, err := ImageImport(ctx, a, comp, "staging-bkt", "test-ns", "app-1.qcow2", "app-alpine-1", "Linux", "Alpine (self-supported)")
			return err
		},
		"ImageWaitReady": func(ctx context.Context, a CloudAuth) error {
			return ImageWaitReady(ctx, a, "ocid1.image.test")
		},
		"ImageGet": func(ctx context.Context, a CloudAuth) error {
			_, err := ImageGet(ctx, a, "ocid1.image.test")
			return err
		},
		"ImageDeleteStage": func(ctx context.Context, a CloudAuth) error {
			return ImageDeleteStage(ctx, a, "staging-bkt", "app-1.qcow2")
		},
	}
}

// A gcp profile refuses per op with the EXACT Cloud Build message, and not
// one call lands on either CLI (the refusal fires before any argv exists).
func TestImageOpsGCPFailsClosed(t *testing.T) {
	gcpLog, ociLog := setupBGDispatchShims(t)
	for name, op := range imageOpsAll() {
		err := op(context.Background(), CloudAuth{Provider: "gcp"})
		if err == nil || err.Error() != wantImageImportRefusal {
			t.Errorf("%s(gcp) = %v, want the exact Cloud Build refusal", name, err)
		}
	}
	if calls := bgOpShimCalls(t, ociLog); len(calls) != 0 {
		t.Errorf("the refusals must not exec oci: %v", calls)
	}
	if calls := bgOpShimCalls(t, gcpLog); len(calls) != 0 {
		t.Errorf("the refusals must not exec gcloud: %v", calls)
	}
}

// The refusal const is the ONE source of truth: it must equal the pinned
// literal the command-side entry-guard test asserts on.
func TestImageImportGCPRefusalConstIsTheContract(t *testing.T) {
	if ImageImportGCPRefusal != wantImageImportRefusal {
		t.Errorf("ImageImportGCPRefusal = %q, want the pinned contract text", ImageImportGCPRefusal)
	}
	if strings.Count(ImageImportGCPRefusal, "Cloud Build") != 1 {
		t.Errorf("refusal text lost its Cloud Build remedy: %q", ImageImportGCPRefusal)
	}
}
