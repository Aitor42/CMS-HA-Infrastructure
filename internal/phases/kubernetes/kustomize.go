package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strings"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func copyKustomizeTree(ctx context.Context, pool ssh.Executor, host, dest string) error {
	tarball, err := buildKustomizeTar()
	if err != nil {
		return err
	}
	if err := pool.CopyTar(ctx, host, dest, tarball); err != nil {
		return fmt.Errorf("extract kustomize tree: %w", err)
	}
	return nil
}

// buildKustomizeTar packs the embedded kubernetes/ tree into a tar archive with
// deterministic modes (directories 0700, regular files 0644).
func buildKustomizeTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := fs.WalkDir(cms.KubernetesFS, "kubernetes", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "kubernetes/")
		if rel == "" || rel == "kubernetes" {
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Mode: 0700, Typeflag: tar.TypeDir})
		}
		content, err := cms.KubernetesFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded manifest %s: %w", p, err)
		}
		hdr := &tar.Header{Name: rel, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		_ = tw.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
