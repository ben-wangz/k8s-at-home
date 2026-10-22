package support

import (
	"archive/tar"
	"bytes"
)

const resumeFixtureFileSize = 4 << 20
const s3ResumeFixtureFileSize = 6 << 20

func BuildResumeLayerFixture(mode string) ([]byte, string) {
	var body bytes.Buffer
	archive := tar.NewWriter(&body)
	fileSize := resumeFixtureFileSize
	if mode == "s3" {
		fileSize = s3ResumeFixtureFileSize
	}
	pattern := []byte(mode + "-")
	content := bytes.Repeat(pattern, (fileSize+len(pattern)-1)/len(pattern))[:fileSize]
	_ = archive.WriteHeader(&tar.Header{Name: "registry-hub-resume.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = archive.Write(content)
	_ = archive.Close()
	return body.Bytes(), Digest(body.Bytes())
}
