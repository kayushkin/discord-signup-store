package discordsignup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FileStoreClient keeps the photos people upload for their avatars, and for
// their servers' mascots, in file-store, which owns uploaded bytes. This
// service owns which photo is whose and who may see it; file-store holds the
// bytes and judges nobody. Its
// token reads every file there, so it comes from file-store's own host-local
// file and reaches this unit through a drop-in, never the tracked unit.
type FileStoreClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// fileStoreOwnerService is how this service names itself as a file's owner.
const fileStoreOwnerService = "discord-signup-store"

// ErrFileStoreNotConfigured is a photo upload on a service started without
// file-store's address or token.
var ErrFileStoreNotConfigured = errors.New("file-store is not configured (FILE_STORE_URL and FILE_STORE_SERVICE_TOKEN)")

// NewFileStoreClient returns nil when either value is empty: the avatar
// routes then say file-store is not configured, and nothing else changes.
func NewFileStoreClient(baseURL, token string) *FileStoreClient {
	if baseURL == "" || token == "" {
		return nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &FileStoreClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Transport: transport}}
}

func (c *FileStoreClient) do(request *http.Request) (*http.Response, error) {
	request.Header.Set("X-File-Store-Service-Token", c.token)
	return c.http.Do(request)
}

func fileStoreRefusal(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("file-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
}

// UploadAvatarPhoto stores one person's photo and returns its file id.
func (c *FileStoreClient) UploadAvatarPhoto(discordUserID, filename, contentType string, content []byte) (string, error) {
	return c.uploadPhoto("avatar:"+discordUserID, filename, contentType, content)
}

// UploadMascotPhoto stores the photo a server's mascot is to be drawn from
// and returns its file id.
func (c *FileStoreClient) UploadMascotPhoto(guildID, filename, contentType string, content []byte) (string, error) {
	return c.uploadPhoto("mascot:"+guildID, filename, contentType, content)
}

// uploadPhoto stores a photo under ownerRef, which says what it hangs on.
func (c *FileStoreClient) uploadPhoto(ownerRef, filename, contentType string, content []byte) (string, error) {
	query := url.Values{"filename": {filename}, "owner_service": {fileStoreOwnerService}, "owner_ref": {ownerRef}}
	request, err := http.NewRequest(http.MethodPost, c.baseURL+"/files?"+query.Encode(), bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", contentType)
	response, err := c.do(request)
	if err != nil {
		return "", fmt.Errorf("upload to file-store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fileStoreRefusal(response)
	}
	var file struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&file); err != nil {
		return "", fmt.Errorf("file-store answered an upload that does not parse: %w", err)
	}
	if file.ID == "" {
		return "", errors.New("file-store answered an upload with no id")
	}
	return file.ID, nil
}

// PhotoContent is the bytes of one stored photo and their declared type.
func (c *FileStoreClient) PhotoContent(fileID string) ([]byte, string, error) {
	request, err := http.NewRequest(http.MethodGet, c.baseURL+"/files/"+url.PathEscape(fileID)+"/content", nil)
	if err != nil {
		return nil, "", err
	}
	response, err := c.do(request)
	if err != nil {
		return nil, "", fmt.Errorf("read from file-store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("%w: file-store has no file %s", ErrNotFound, fileID)
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", fileStoreRefusal(response)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read from file-store: %w", err)
	}
	// file-store serves every file as an attachment of octet-stream unless
	// asked for inline, so the type comes from the bytes, as it did on upload.
	return content, http.DetectContentType(content), nil
}

// PurgePhoto destroys a photo, row and bytes. A photo file-store no longer
// has is already what this asks for.
func (c *FileStoreClient) PurgePhoto(fileID string) error {
	request, err := http.NewRequest(http.MethodDelete, c.baseURL+"/files/"+url.PathEscape(fileID)+"?hard=true", nil)
	if err != nil {
		return err
	}
	response, err := c.do(request)
	if err != nil {
		return fmt.Errorf("purge from file-store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fileStoreRefusal(response)
	}
	return nil
}
