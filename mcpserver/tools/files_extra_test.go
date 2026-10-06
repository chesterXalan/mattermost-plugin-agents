// Copyright (c) 2023-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileExtraToolsValidation(t *testing.T) {
	provider := newTestProvider(t, "https://mm.example.com")
	client := newTestClient("https://mm.example.com")
	mcpCtx := &MCPToolContext{Client: client, Ctx: t.Context(), AccessMode: AccessModeRemote}
	// Local access mode: upload_file passes the access-mode gate, so downstream
	// validation (e.g. empty path) is exercised.
	localCtx := &MCPToolContext{Client: client, Ctx: t.Context(), AccessMode: AccessModeLocal}

	tests := []struct {
		name    string
		call    func() (string, error)
		wantErr string
	}{
		{"get_file_info bad", func() (string, error) { return provider.toolGetFileInfo(mcpCtx, GetFileInfoArgs{FileID: "bad"}) }, "must be a valid ID"},
		{"get_post_files bad", func() (string, error) { return provider.toolGetPostFiles(mcpCtx, GetPostFilesArgs{PostID: "bad"}) }, "must be a valid ID"},
		{"get_file_link bad", func() (string, error) { return provider.toolGetFileLink(mcpCtx, GetFileLinkArgs{FileID: "bad"}) }, "must be a valid ID"},
		{"search_files empty", func() (string, error) {
			return provider.toolSearchFiles(mcpCtx, SearchFilesArgs{Terms: "", TeamID: model.NewId()})
		}, "terms cannot be empty"},
		{"upload_file empty path", func() (string, error) {
			return provider.toolUploadFile(localCtx, UploadFileArgs{ChannelID: model.NewId(), Path: ""})
		}, "path cannot be empty"},
		{"download_file bad", func() (string, error) {
			return provider.toolDownloadFile(localCtx, DownloadFileArgs{FileID: "bad"})
		}, "must be a valid ID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestToolUploadFileRemoteGating(t *testing.T) {
	provider := newTestProvider(t, "https://mm.example.com")
	client := newTestClient("https://mm.example.com")
	mcpCtx := &MCPToolContext{Client: client, Ctx: t.Context(), AccessMode: AccessModeRemote}

	// In remote mode, upload returns a graceful (non-error) message.
	out, err := provider.toolUploadFile(mcpCtx, UploadFileArgs{ChannelID: model.NewId(), Path: "report.pdf"})
	require.NoError(t, err)
	assert.Contains(t, out, "local access mode")
}

func TestToolDownloadFileRemoteGating(t *testing.T) {
	provider := newTestProvider(t, "https://mm.example.com")
	client := newTestClient("https://mm.example.com")
	mcpCtx := &MCPToolContext{Client: client, Ctx: t.Context(), AccessMode: AccessModeRemote}

	// In remote mode, download returns a graceful (non-error) message.
	out, err := provider.toolDownloadFile(mcpCtx, DownloadFileArgs{FileID: model.NewId()})
	require.NoError(t, err)
	assert.Contains(t, out, "local access mode")
}

func TestToolDownloadFile(t *testing.T) {
	fileID := model.NewId()
	fileData := []byte{0x50, 0x4B, 0x03, 0x04, 0x00, 0xFF}
	downloadDir := t.TempDir()
	dataDir := t.TempDir()

	originalGetDataDirectory := GetDataDirectoryInternal
	GetDataDirectoryInternal = func() (string, error) { return dataDir, nil }
	t.Cleanup(func() { GetDataDirectoryInternal = originalGetDataDirectory })

	tests := []struct {
		name        string
		info        *model.FileInfo
		downloadDir string // DownloadDirEnvVar value; empty leaves it unset
		wantPath    string
		wantErr     string
	}{
		{
			name:        "file is saved under its original name",
			info:        &model.FileInfo{Id: fileID, Name: "sheet.xlsx", Size: int64(len(fileData))},
			downloadDir: downloadDir,
			wantPath:    filepath.Join(downloadDir, fileID, "sheet.xlsx"),
		},
		{
			name:     "download directory defaults to the data directory",
			info:     &model.FileInfo{Id: fileID, Name: "sheet.xlsx", Size: int64(len(fileData))},
			wantPath: filepath.Join(dataDir, "downloads", fileID, "sheet.xlsx"),
		},
		{
			name:        "path components in the file name are dropped",
			info:        &model.FileInfo{Id: fileID, Name: "../../evil.sh", Size: int64(len(fileData))},
			downloadDir: downloadDir,
			wantPath:    filepath.Join(downloadDir, fileID, "evil.sh"),
		},
		{
			name:        "unusable file name falls back to a default",
			info:        &model.FileInfo{Id: fileID, Name: "..", Size: int64(len(fileData))},
			downloadDir: downloadDir,
			wantPath:    filepath.Join(downloadDir, fileID, "attachment"),
		},
		{
			name:        "file over the size limit is rejected",
			info:        &model.FileInfo{Id: fileID, Name: "huge.bin", Size: maxMCPFetchBytes + 1},
			downloadDir: downloadDir,
			wantErr:     "larger than",
		},
		{
			name:        "relative download directory is rejected",
			info:        &model.FileInfo{Id: fileID, Name: "sheet.xlsx", Size: int64(len(fileData))},
			downloadDir: "relative/dir",
			wantErr:     "must be an absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(DownloadDirEnvVar, tt.downloadDir)
			server := newTestFileServer(t, tt.info, fileData)
			provider := newTestProvider(t, server.URL)
			mcpCtx := &MCPToolContext{Client: newTestClient(server.URL), Ctx: t.Context(), AccessMode: AccessModeLocal}

			out, err := provider.toolDownloadFile(mcpCtx, DownloadFileArgs{FileID: fileID})
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out, tt.wantPath)

			saved, readErr := os.ReadFile(tt.wantPath)
			require.NoError(t, readErr)
			assert.Equal(t, fileData, saved)
		})
	}
}

func TestToolGetFileInfo(t *testing.T) {
	fileID := model.NewId()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v4/files/%s/info", fileID), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&model.FileInfo{Id: fileID, Name: "report.pdf", MimeType: "application/pdf", Size: 1024})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	provider := newTestProvider(t, ts.URL)
	mcpCtx := &MCPToolContext{Client: newTestClient(ts.URL), Ctx: t.Context()}

	out, err := provider.toolGetFileInfo(mcpCtx, GetFileInfoArgs{FileID: fileID})
	require.NoError(t, err)
	assert.Contains(t, out, "report.pdf")
	assert.Contains(t, out, fileID)
}

func TestToolGetFileLink(t *testing.T) {
	fileID := model.NewId()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v4/files/%s/link", fileID), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"link": "https://mm.example.com/files/public/abc"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	provider := newTestProvider(t, ts.URL)
	mcpCtx := &MCPToolContext{Client: newTestClient(ts.URL), Ctx: t.Context()}

	out, err := provider.toolGetFileLink(mcpCtx, GetFileLinkArgs{FileID: fileID})
	require.NoError(t, err)
	assert.Contains(t, out, "https://mm.example.com/files/public/abc")
}
