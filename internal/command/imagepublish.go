package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

// vm image — the golden-image publish/verify surface over the manifest
// contract (goldenmanifest.go): publish = digest the REAL bytes + put
// artifact+manifest into golden-image/<name>-<date>-<digest8>/ (the
// layout makes overwrites structurally impossible and "latest"
// unrepresentable); list = the bucket inventory; verify = manifest +
// byte-level digest check. Cloud auth rides the instance profile's
// cloud block, exactly like image-import.
const vmImageHelp = `Usage:
  kampodra vm image publish --image <qcow2> --name <name> [--source-url <url>] [--bucket <b>]
  kampodra vm image list   [--bucket <b>]
  kampodra vm image verify --dir golden-image/<name>-<date>-<digest8> --artifact <local-file>
`

func newVMImageCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "golden-image publish/list/verify (digest-pinned, never latest)",
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), vmImageHelp)
	})
	pf := cmd.PersistentFlags()
	pf.String("profile", "", "kampodra instance profile (project naming + cloud block)")
	pf.String("bucket", "", "staging bucket (KAMPODRA_IMPORT_BUCKET env, else <container>-image-import)")
	pf.String("compartment", "", "compartment name or ocid (cloud block or KAMPODRA_CLOUD_COMPARTMENT env)")
	cmd.AddCommand(newVMImagePublishCommand(d), newVMImageListCommand(d), newVMImageVerifyCommand(d))
	return cmd
}

// imagePublishStage resolves the publish inputs: the artifact must exist,
// the name must be layout-safe, the auth/bucket pair resolved.
type imagePublishStage struct {
	artifact  string
	name      string
	sourceURL string
	bucket    string
	auth      cloud.CloudAuth
}

func stageImagePublish(d Deps, c *cobra.Command) (imagePublishStage, error) {
	var st imagePublishStage
	st.artifact = flagString(c, "image")
	if st.artifact == "" {
		return st, fmt.Errorf("--image <qcow2> is required")
	}
	if _, err := os.Stat(st.artifact); err != nil {
		return st, fmt.Errorf("cannot read --image %s: %w", st.artifact, err)
	}
	st.name = flagString(c, "name")
	if st.name == "" {
		return st, fmt.Errorf("--name <name> is required (e.g. ubuntu-24.04-minimal)")
	}
	if strings.ContainsAny(st.name, "/ \\:") || strings.Contains(st.name, "..") {
		return st, fmt.Errorf("--name must be layout-safe (no separators, no ..)")
	}
	st.sourceURL = flagString(c, "source-url")
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return st, err
	}
	pair, err := resolveBGPair(d, target, flagString(c, "compartment"), surfaceImageImport)
	if err != nil {
		return st, err
	}
	st.auth = pair.auth
	st.bucket = flagString(c, "bucket")
	if st.bucket == "" {
		st.bucket = os.Getenv("KAMPODRA_IMPORT_BUCKET")
	}
	if st.bucket == "" {
		st.bucket = pair.container + "-image-import"
	}
	return st, nil
}

func newVMImagePublishCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish --image <qcow2> --name <name>",
		Short: "digest + upload a golden image with its provenance manifest",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := stageImagePublish(d, c)
			if err != nil {
				return err
			}
			return runVMImagePublish(d, c.Context(), st)
		},
	}
	cmd.Flags().String("image", "", "the image artifact (required)")
	cmd.Flags().String("name", "", "layout-safe image name (required)")
	cmd.Flags().String("source-url", "", "where the artifact came from (provenance)")
	return cmd
}

func runVMImagePublish(d Deps, ctx context.Context, st imagePublishStage) error {
	m, manifestJSON, err := EmitGoldenManifest(st.artifact, st.name, st.sourceURL, "kampodra")
	if err != nil {
		return err
	}
	dir := GoldenImageDir(st.name, m.Digest, m.ImportedAt)
	fmt.Fprintf(d.Stdout, "[image] digest %s… (%d bytes)\n", m.Digest[:12], m.SizeBytes)

	// never-overwrite guard: if the manifest already lives in the digest
	// directory, the image is published — verify, never re-put.
	headOut, err := cloud.RunCloud(ctx, st.auth, cloud.ObjectHeadArgs(st.bucket, dir+"/"+ManifestFileName, "", st.auth.Profile, st.auth.InstancePrincipal))
	if err == nil && strings.Contains(headOut, "content-length") {
		fmt.Fprintf(d.Stdout, "[image] %s is already published — nothing to do (verify it: kampodra vm image verify --dir %s --artifact <local>)\n", dir, dir)
		return nil
	}

	if err := cloud.ImageStage(ctx, st.auth, st.bucket, st.artifact, dir+"/"+filepath.Base(st.artifact)); err != nil {
		return fmt.Errorf("artifact upload failed (bucket exists? oci os bucket create -bn %s): %w", st.bucket, err)
	}
	tmpManifest := filepath.Join(os.TempDir(), "kampodra-manifest-"+m.Digest[:8]+".json")
	if err := os.WriteFile(tmpManifest, []byte(manifestJSON), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmpManifest)
	if err := cloud.ImageStage(ctx, st.auth, st.bucket, tmpManifest, dir+"/"+ManifestFileName); err != nil {
		return fmt.Errorf("manifest upload failed: %w", err)
	}
	fmt.Fprintf(d.Stdout, "[image] published os://%s/%s (pin the digest: %s)\n", st.bucket, dir, m.Digest)
	return nil
}

func newVMImageListCommand(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "inventory the golden-image prefix (digest-pinned dirs)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := stageImagePublish(d, c)
			if err != nil {
				return err
			}
			out, err := cloud.RunCloud(c.Context(), st.auth, cloud.ObjectListArgs(st.bucket, "golden-image/", "", st.auth.Profile, st.auth.InstancePrincipal))
			if err != nil {
				return fmt.Errorf("bucket inventory failed: %w", err)
			}
			var parsed struct {
				Data []struct {
					Name string `json:"name"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				return fmt.Errorf("unexpected bucket listing: %w", err)
			}
			seen := map[string]bool{}
			for _, o := range parsed.Data {
				dir := filepath.Dir(o.Name)
				if dir != "." && !seen[dir] {
					seen[dir] = true
					fmt.Fprintln(d.Stdout, dir)
				}
			}
			return nil
		},
	}
}

func newVMImageVerifyCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify --dir <golden-image/…> --artifact <local-file>",
		Short: "check a local artifact against its published manifest",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir := flagString(c, "dir")
			artifact := flagString(c, "artifact")
			if dir == "" || artifact == "" {
				return fmt.Errorf("--dir <golden-image/…> and --artifact <local-file> are both required")
			}
			st, err := stageImagePublish(d, c)
			if err != nil {
				return err
			}
			ctx := c.Context()
			tmp, err := os.CreateTemp("", "kampodra-verify-*.json")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			tmp.Close()
			if _, err := cloud.RunCloud(ctx, st.auth, cloud.ObjectGetArgs(st.bucket, dir+"/"+ManifestFileName, tmp.Name(), "", st.auth.Profile, st.auth.InstancePrincipal)); err != nil {
				return fmt.Errorf("cannot fetch the manifest sidecar from os://%s/%s: %w", st.bucket, dir, err)
			}
			data, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			m, err := ParseGoldenManifest(data)
			if err != nil {
				return err
			}
			if err := VerifyGoldenArtifact(artifact, m); err != nil {
				return err
			}
			fmt.Fprintf(d.Stdout, "[image] VERIFIED %s: %s… (%d bytes) matches the manifest (%s, imported %s)\n", dir, m.Digest[:12], m.SizeBytes, m.Name, m.ImportedAt.Format("2006-01-02"))
			return nil
		},
	}
	cmd.Flags().String("dir", "", "the digest-pinned directory under the bucket's golden-image/ prefix (required)")
	cmd.Flags().String("artifact", "", "the local artifact file to verify (required)")
	return cmd
}
