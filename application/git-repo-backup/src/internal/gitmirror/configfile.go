package gitmirror

import (
	"context"
	"fmt"
	"strings"
)

// PlaceholderRemoteURL replaces the real source URL inside archived mirror
// configs. It is intentionally non-routing and non-sensitive.
const PlaceholderRemoteURL = "ssh://git@restore.invalid/repository.git"

// ConfigInfo carries the mirror-config values preserved during
// normalization. Everything else (includes, credential helpers, URL rewrites,
// hooks, upload execution) is dropped.
type ConfigInfo struct {
	RepositoryFormatVersion string
	ObjectFormat            string
	RemoteURL               string
}

// ReadConfigInfo extracts the storage-format settings of an existing mirror.
func (r *Runner) ReadConfigInfo(ctx context.Context, dir string) (ConfigInfo, error) {
	info := ConfigInfo{RepositoryFormatVersion: "0"}
	out, err := r.Run(ctx, "-C", dir, "config", "--get", "core.repositoryformatversion")
	if err == nil {
		info.RepositoryFormatVersion = strings.TrimSpace(out)
	}
	out, err = r.Run(ctx, "-C", dir, "config", "--get", "extensions.objectformat")
	if err == nil {
		info.ObjectFormat = strings.TrimSpace(out)
	}
	return info, nil
}

// RenderBareConfig writes the minimal controlled bare-mirror config: mirror
// semantics, the origin fetch refspec, and the preserved object format.
func RenderBareConfig(info ConfigInfo) []byte {
	var b strings.Builder
	b.WriteString("[core]\n")
	fmt.Fprintf(&b, "\trepositoryformatversion = %s\n", info.RepositoryFormatVersion)
	b.WriteString("\tfilemode = true\n")
	b.WriteString("\tbare = true\n")
	if info.ObjectFormat != "" {
		b.WriteString("[extensions]\n")
		fmt.Fprintf(&b, "\tobjectformat = %s\n", info.ObjectFormat)
	}
	b.WriteString("[remote \"origin\"]\n")
	fmt.Fprintf(&b, "\turl = %s\n", info.RemoteURL)
	b.WriteString("\tfetch = +refs/*:refs/*\n")
	b.WriteString("\tmirror = true\n")
	return []byte(b.String())
}

// NormalizeConfig replaces the mirror's config with the minimal controlled
// one, preserving format settings and restoring the origin mirror refspec.
func (r *Runner) NormalizeConfig(ctx context.Context, dir, remoteURL string) error {
	info, err := r.ReadConfigInfo(ctx, dir)
	if err != nil {
		return err
	}
	info.RemoteURL = remoteURL
	return writeMirrorConfig(dir, RenderBareConfig(info))
}
