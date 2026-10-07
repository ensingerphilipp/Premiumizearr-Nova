package premiumizeme

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// TransferSourceKind identifies the payload format accepted by Premiumize's
// /transfer/create endpoint.
type TransferSourceKind string

const (
	TransferSourceMagnet      TransferSourceKind = "magnet"
	TransferSourceTorrent     TransferSourceKind = "torrent"
	TransferSourceNZB         TransferSourceKind = "nzb"
	maxTransferSourceSize                        = 100 << 20
	transferSubmissionTimeout                    = 2 * time.Minute
	folderListTimeout                            = 2 * time.Minute
)

type Premiumizeme struct {
	APIKey     string
	APIBaseURL string
	HTTPClient *http.Client
}

func NewPremiumizemeClient(APIKey string) Premiumizeme {
	return Premiumizeme{
		APIKey:     APIKey,
		APIBaseURL: "https://www.premiumize.me/api/",
		HTTPClient: http.DefaultClient,
	}
}

func (pm *Premiumizeme) createPremiumizemeURL(urlPath string) (url.URL, error) {
	baseURL := pm.APIBaseURL
	if baseURL == "" {
		baseURL = "https://www.premiumize.me/api/"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return *u, err
	}
	u.Path = path.Join(u.Path, urlPath)
	q := u.Query()
	q.Set("apikey", pm.APIKey)
	u.RawQuery = q.Encode()
	return *u, nil
}

func (pm *Premiumizeme) httpClient() *http.Client {
	if pm.HTTPClient != nil {
		return pm.HTTPClient
	}
	return http.DefaultClient
}

func (pm *Premiumizeme) GetAccountInfo() (AccountInfoResponse, error) {
	var accountInfo AccountInfoResponse
	if pm.APIKey == "" {
		return accountInfo, ErrAPIKeyNotSet
	}

	accountURL, err := pm.createPremiumizemeURL("/account/info")
	if err != nil {
		return accountInfo, err
	}

	request, err := http.NewRequest(http.MethodGet, accountURL.String(), nil)
	if err != nil {
		return accountInfo, pm.accountInfoRequestError(err)
	}

	response, err := pm.httpClient().Do(request)
	if err != nil {
		return accountInfo, pm.accountInfoRequestError(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return accountInfo, fmt.Errorf("account info request failed: %s (%d)", response.Status, response.StatusCode)
	}

	if err := json.NewDecoder(response.Body).Decode(&accountInfo); err != nil {
		return accountInfo, err
	}
	if accountInfo.Status != "success" {
		return accountInfo, pm.accountInfoRequestError(fmt.Errorf("%s: %s", accountInfo.Status, accountInfo.Message))
	}

	return accountInfo, nil
}

func (pm *Premiumizeme) accountInfoRequestError(err error) error {
	message := err.Error()
	for _, secret := range []string{pm.APIKey, url.QueryEscape(pm.APIKey), url.PathEscape(pm.APIKey)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return fmt.Errorf("account info request failed: %s", message)
}

// redactRequestError removes the premiumize.me API key from a request error
// before it is returned to callers: the request URL carries the key as a
// query parameter, so a raw *url.Error would print it into logs.
func (pm *Premiumizeme) redactRequestError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range []string{pm.APIKey, url.QueryEscape(pm.APIKey), url.PathEscape(pm.APIKey)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return fmt.Errorf("%s", message)
}

var (
	ErrAPIKeyNotSet = fmt.Errorf("premiumize.me API key not set")
)

func (pm *Premiumizeme) GetTransfers() ([]Transfer, error) {
	if pm.APIKey == "" {
		return nil, ErrAPIKeyNotSet
	}

	log.Trace("Getting transfers list from premiumize.me")
	url, err := pm.createPremiumizemeURL("/transfer/list")
	if err != nil {
		return nil, err
	}

	var ret []Transfer
	req, _ := http.NewRequest("GET", url.String(), nil)

	resp, err := pm.httpClient().Do(req)
	if err != nil {
		return ret, pm.redactRequestError(err)
	}

	defer resp.Body.Close()
	res := ListTransfersResponse{}
	err = json.NewDecoder(resp.Body).Decode(&res)

	if res.Status != "success" {
		return ret, fmt.Errorf("%s", res.Status)
	}

	if err != nil {
		return ret, err
	}

	log.Tracef("Received %d transfers", len(res.Transfers))
	return res.Transfers, nil
}

// ListFolder lists the items of one Premiumize folder. It keeps the
// historical signature for existing callers; call sites that own a context
// should use ListFolderContext so a hung listing can be interrupted.
func (pm *Premiumizeme) ListFolder(folderID string) ([]Item, error) {
	return pm.ListFolderContext(context.Background(), folderID)
}

// ListFolderContext is the context-aware variant of ListFolder: cancelling
// the context interrupts a hanging listing, and the request additionally
// carries a bounded timeout because a hang is not a slow response.
func (pm *Premiumizeme) ListFolderContext(ctx context.Context, folderID string) ([]Item, error) {
	if pm.APIKey == "" {
		return nil, ErrAPIKeyNotSet
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var ret []Item
	url, err := pm.createPremiumizemeURL("/folder/list")
	if err != nil {
		return ret, err
	}

	q := url.Query()
	q.Set("id", folderID)
	url.RawQuery = q.Encode()

	requestContext, cancel := context.WithTimeout(ctx, folderListTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, "GET", url.String(), nil)
	if err != nil {
		return ret, err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return ret, pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return ret, fmt.Errorf("error listing folder: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	res := ListFoldersResponse{}
	log.Trace("List Folder: Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	if err != nil {
		return ret, err
	}

	if res.Status != "success" {
		return ret, fmt.Errorf(res.Message)
	}

	return res.Content, nil
}

func (pm *Premiumizeme) GetFolders() ([]Item, error) {
	if pm.APIKey == "" {
		return nil, ErrAPIKeyNotSet
	}

	log.Trace("Getting folder list from premiumize.me")
	url, err := pm.createPremiumizemeURL("/folder/list")
	if err != nil {
		return nil, err
	}

	var ret []Item
	req, _ := http.NewRequest("GET", url.String(), nil)

	resp, err := pm.httpClient().Do(req)
	if err != nil {
		return ret, pm.redactRequestError(err)
	}

	defer resp.Body.Close()
	res := ListFoldersResponse{}
	err = json.NewDecoder(resp.Body).Decode(&res)

	if res.Status != "success" {
		return ret, fmt.Errorf("%s", res.Status)
	}

	if err != nil {
		return ret, err
	}

	log.Tracef("Received %d Folders", len(res.Content))
	return res.Content, nil
}

func (pm *Premiumizeme) CreateTransfer(filePath string, parentID string) error {
	if pm.APIKey == "" {
		return ErrAPIKeyNotSet
	}

	//TODO: handle file size, i.e. incorrect file being saved
	log.Trace("Opening file: ", filePath)
	file, err := os.Open(filePath)
	if err != nil {
		log.Errorf("First try failed, waiting 1 second and trying to open file: %s again", filePath)
		time.Sleep(1 * time.Second)
		file, err = os.Open(filePath)
		if err != nil {
			return err
		}
	}
	defer file.Close()

	url, err := pm.createPremiumizemeURL("/transfer/create")
	if err != nil {
		return err
	}

	var request *http.Request

	switch filepath.Ext(file.Name()) {
	case ".nzb":
		request, err = createNZBRequest(file, &url, parentID)
	case ".magnet":
		request, err = createMagnetRequest(file, &url, parentID)
	case ".torrent":
		request, err = createTorrentRequest(file, &url, parentID)
	}

	if err != nil {
		return err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("error creating transfer: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	res := CreateTransferResponse{}
	log.Trace("CreateTransfer: Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	if err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf(res.Message)
	}

	log.Tracef("Transfer created: %+v", res)

	return nil
}

// CreateTransferFromBytes submits a transfer payload without requiring a
// temporary file. name is used as the uploaded filename for torrent and NZB
// sources; magnet sources are sent as a multipart text field. The returned
// response includes Premiumize's transfer ID for subsequent tracking.
func (pm *Premiumizeme) CreateTransferFromBytes(ctx context.Context, kind TransferSourceKind, data []byte, name, parentID string) (CreateTransferResponse, error) {
	var result CreateTransferResponse
	if pm.APIKey == "" {
		return result, ErrAPIKeyNotSet
	}
	if ctx == nil {
		return result, fmt.Errorf("transfer context is nil")
	}
	if len(data) == 0 || len(data) > maxTransferSourceSize {
		return result, fmt.Errorf("transfer source size must be between 1 and %d bytes", maxTransferSourceSize)
	}
	if kind != TransferSourceMagnet && kind != TransferSourceTorrent && kind != TransferSourceNZB {
		return result, fmt.Errorf("unsupported transfer source kind %q", kind)
	}
	if kind == TransferSourceMagnet && len(data) > 16*1024 {
		return result, fmt.Errorf("magnet source exceeds 16384 bytes")
	}
	if kind != TransferSourceMagnet {
		name = filepath.Base(name)
		if name == "." || name == string(filepath.Separator) || name == "" {
			return result, fmt.Errorf("transfer filename is required")
		}
	}

	endpoint, err := pm.createPremiumizemeURL("/transfer/create")
	if err != nil {
		return result, err
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	var part io.Writer
	if kind == TransferSourceMagnet {
		part, err = writer.CreateFormField("src")
	} else {
		part, err = writer.CreateFormFile("src", name)
	}
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		part, err = writer.CreateFormField("folder_id")
	}
	if err == nil {
		_, err = io.WriteString(part, parentID)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return result, err
	}
	requestContext, cancel := context.WithTimeout(ctx, transferSubmissionTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return result, pm.redactRequestError(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return result, pm.redactRequestError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("error creating transfer: %s (%d)", resp.Status, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CreateTransferResponse{}, err
	}
	if result.Status != "success" {
		return CreateTransferResponse{}, pm.redactRequestError(fmt.Errorf("transfer creation failed: %s", result.Message))
	}
	if result.ID == "" {
		return CreateTransferResponse{}, fmt.Errorf("transfer creation response did not include an ID")
	}
	return result, nil
}

func (pm *Premiumizeme) DeleteFolder(folderID string) error {
	if pm.APIKey == "" {
		return ErrAPIKeyNotSet
	}

	url, err := pm.createPremiumizemeURL("/folder/delete")
	if err != nil {
		return err
	}

	q := url.Query()
	q.Set("id", folderID)
	url.RawQuery = q.Encode()

	request, err := http.NewRequest("DELETE", url.String(), nil)
	if err != nil {
		return err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("error deleting folder: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	res := SimpleResponse{}
	log.Trace("DeleteFolder Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	if err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf(res.Message)
	}

	log.Tracef("Folder deleted: %+v", res)

	return nil
}

func (pm *Premiumizeme) MoveItem(itemID string, folderID string) error {
	if pm.APIKey == "" {
		return ErrAPIKeyNotSet
	}

	url, err := pm.createPremiumizemeURL("/folder/paste")
	if err != nil {
		return err
	}

	q := url.Query()
	q.Set("files[]", itemID)
	q.Set("id", folderID)
	url.RawQuery = q.Encode()

	request, err := http.NewRequest("POST", url.String(), nil)
	if err != nil {
		return err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("error moving single item to folder: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	res := SimpleResponse{}
	log.Trace("MoveItem Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	if err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf(res.Message)
	}

	log.Tracef("Item moved: %+v", res)

	return nil
}

func (pm *Premiumizeme) CreateFolder(folderName string, parentID *string) (string, error) {
	if pm.APIKey == "" {
		return "", ErrAPIKeyNotSet
	}

	url, err := pm.createPremiumizemeURL("/folder/create")
	if err != nil {
		return "", err
	}

	q := url.Query()
	q.Set("name", folderName)
	if parentID != nil {
		q.Set("parent_id", *parentID)
	}
	url.RawQuery = q.Encode()

	request, err := http.NewRequest("POST", url.String(), nil)
	if err != nil {
		return "", err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return "", pm.redactRequestError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error creating folder: %s (%d)", resp.Status, resp.StatusCode)
	}

	res := CreateFolderResponse{}
	log.Trace("CreateFolder Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return "", err
	}

	if res.Status != "success" {
		return "", fmt.Errorf(res.Message)
	}

	log.Tracef("Folder created: %+v", res)
	return res.ID, nil
}

func (pm *Premiumizeme) DeleteTransfer(id string) error {
	if pm.APIKey == "" {
		return ErrAPIKeyNotSet
	}

	url, err := pm.createPremiumizemeURL("/transfer/delete")
	if err != nil {
		return err
	}

	request, err := createDeleteRequest(id, &url)
	if err != nil {
		return err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("error deleting transfer: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	res := SimpleResponse{}
	log.Trace("DeleteTransfer Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	if err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf("failed to delete transfer: %s, message: %+v", id, res.Message)
	}

	log.Tracef("Transfer Deleted: %+v", res)

	return nil
}

func createNZBRequest(file *os.File, url *url.URL, parentID string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("src", filepath.Base(file.Name()))

	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, err
	}

	part, err = writer.CreateFormField("folder_id")

	if err != nil {
		return nil, err
	}

	_, err = part.Write([]byte(parentID))

	if err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	request, err := http.NewRequest("POST", url.String(), body)

	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", writer.FormDataContentType())

	return request, nil
}

func createMagnetRequest(file *os.File, url *url.URL, parentID string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormField("src")

	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, err
	}

	part, err = writer.CreateFormField("folder_id")

	if err != nil {
		return nil, err
	}

	_, err = part.Write([]byte(parentID))

	if err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	request, err := http.NewRequest("POST", url.String(), body)

	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", writer.FormDataContentType())

	return request, nil
}

func createTorrentRequest(file *os.File, url *url.URL, parentID string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("src", filepath.Base(file.Name()))

	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, err
	}

	part, err = writer.CreateFormField("folder_id")

	if err != nil {
		return nil, err
	}

	_, err = part.Write([]byte(parentID))

	if err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	request, err := http.NewRequest("POST", url.String(), body)

	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", writer.FormDataContentType())

	return request, nil
}

func createDeleteRequest(id string, URL *url.URL) (*http.Request, error) {
	// Build Values to send to endpoint
	data := url.Values{}
	data.Set("id", id)

	// Create and encode request
	request, err := http.NewRequest("POST", URL.String(), strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	// Setup headers
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

	return request, nil
}

type SRCType = int

const (
	SRC_FILE = iota
	SRC_FOLDER
)

func (pm *Premiumizeme) GenerateZippedFileLink(fileID string) (string, error) {
	dlLink, err := pm.generateZip(fileID, SRC_FILE)
	if err != nil {
		return "", err
	}
	return dlLink, nil
}

func (pm *Premiumizeme) GenerateZippedFolderLink(fileID string) (string, error) {
	dlLink, err := pm.generateZip(fileID, SRC_FOLDER)
	if err != nil {
		return "", err
	}
	return dlLink, nil
}

func (pm *Premiumizeme) generateZip(ID string, srcType SRCType) (string, error) {
	if pm.APIKey == "" {
		return "", ErrAPIKeyNotSet
	}

	// Build URL with apikey
	URL, err := pm.createPremiumizemeURL("/zip/generate")
	if err != nil {
		return "", err
	}

	// Build Values to send to endpoint
	data := url.Values{}

	if srcType == SRC_FILE {
		data.Set("files[]", ID)
	} else if srcType == SRC_FOLDER {
		data.Set("folders[]", ID)
	} else {
		return "", fmt.Errorf("unknown source type: %d", srcType)
	}

	// Create and encode request
	request, err := http.NewRequest("POST", URL.String(), strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}

	// Setup headers
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

	// Fire request
	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return "", pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error getting zip link for: %s response: %s (%d)", ID, resp.Status, resp.StatusCode)
	}

	// Decode response
	defer resp.Body.Close()
	var res GenerateZipResponse
	log.Trace("generateZip Reading response")
	err = json.NewDecoder(resp.Body).Decode(&res)

	log.Tracef("Zip Response: %+v", res)
	if err != nil {
		return "", err
	}

	if res.Status != "success" {
		return "", fmt.Errorf("error getting zip link for: %s, Status: %s", ID, res.Status)
	}

	log.Debugf("Zip link created: %+v", res.Location)

	return res.Location, nil
}

func (pm *Premiumizeme) GenerateFileLink(ID string) (string, error) {
	if pm.APIKey == "" {
		return "", ErrAPIKeyNotSet
	}

	// Build URL with apikey
	log.Trace("Getting Download Link for Item: ", ID)
	url, err := pm.createPremiumizemeURL("/item/details")
	if err != nil {
		return "", err
	}

	// Build Values to send to endpoint
	q := url.Query()
	q.Set("id", ID)
	url.RawQuery = q.Encode()

	request, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return "", err
	}

	resp, err := pm.httpClient().Do(request)
	if err != nil {
		return "", pm.redactRequestError(err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Error listing Item Details: %s (%d)", resp.Status, resp.StatusCode)
	}

	defer resp.Body.Close()
	var res GenerateFileLinkResponse
	err = json.NewDecoder(resp.Body).Decode(&res)

	if res.Type != "file" {
		return "", fmt.Errorf("Item Type was not File: %s", res.Type)
	}

	if err != nil {
		return "Unknown Error: ", err
	}

	// Generated CDN URLs may contain bearer tokens. Never log them.
	log.Debug("File link created")
	return res.Link, nil
}
