package archives

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nyaruka/null/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadToS3Multipart(t *testing.T) {
	ctx, rt := setup(t)

	// shrink the limits so a small file goes up in parts (S3's minimum part size is 5MB)
	defer func(single, chunk int64) { maxSingleUploadBytes, chunkSizeBytes = single, chunk }(maxSingleUploadBytes, chunkSizeBytes)
	maxSingleUploadBytes, chunkSizeBytes = 6*1024*1024, 5*1024*1024

	data := make([]byte, 12*1024*1024)
	rand.Read(data)
	hash := md5.Sum(data)

	path := filepath.Join(t.TempDir(), "archive.jsonl.gz")
	require.NoError(t, os.WriteFile(path, data, 0600))

	archive := &Archive{ArchiveFile: path, Size: int64(len(data)), Hash: null.String(hex.EncodeToString(hash[:]))}

	err := UploadToS3(ctx, rt.S3, rt.Config.S3Bucket, "test/multipart.jsonl.gz", archive)
	require.NoError(t, err)
	assert.Equal(t, null.String(rt.Config.S3Bucket+":test/multipart.jsonl.gz"), archive.Location)

	size, etag, err := GetS3FileInfo(ctx, rt.S3, rt.Config.S3Bucket, "test/multipart.jsonl.gz")
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	assert.True(t, strings.HasSuffix(etag, "-3"), "expected a 3-part etag, got %s", etag)
}
