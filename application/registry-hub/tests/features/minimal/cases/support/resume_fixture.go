package support

import (
	"archive/tar"
	"bytes"
)

const resumeFixtureFileSize = 4 << 20

func BuildResumeLayerFixture(mode string) ([]byte, string) {
	var body bytes.Buffer
	archive := tar.NewWriter(&body)
	pattern := []byte(mode + "-")
	content := bytes.Repeat(pattern, (resumeFixtureFileSize+len(pattern)-1)/len(pattern))[:resumeFixtureFileSize]
	_ = archive.WriteHeader(&tar.Header{Name: "registry-hub-resume.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = archive.Write(content)
	_ = archive.Close()
	return body.Bytes(), Digest(body.Bytes())
}
