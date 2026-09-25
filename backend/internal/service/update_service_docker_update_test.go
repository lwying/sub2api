//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDockerInAppUpdateReachesVerifiedForkArchive(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("0.2.7")
	client := &updateServiceGitHubClientStub{
		release:      updateTestForkRelease("v0.2.7", archive, checksumsAssetName),
		downloadBody: []byte("unverified archive contents"),
		checksumData: []byte("0000000000000000000000000000000000000000000000000000000000000000  " + archive + "\n"),
	}
	svc := newDockerUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrBinaryUpdateUnsupported)
	require.ErrorContains(t, err, "checksum")
	require.Len(t, client.downloadedURLs(), 1)
}

func TestDockerInAppUpdateRejectsUntrustedForkAssetBeforeDownload(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("0.2.7")
	release := updateTestForkRelease("v0.2.7", archive, checksumsAssetName)
	for i := range release.Assets {
		if release.Assets[i].Name == archive {
			release.Assets[i].BrowserDownloadURL = "https://github.com/Wei-Shaw/sub2api/releases/download/v0.2.7/" + archive
		}
	}
	client := &updateServiceGitHubClientStub{release: release}
	svc := newDockerUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrBinaryUpdateUnsupported)
	require.Empty(t, client.downloadedURLs())
}
