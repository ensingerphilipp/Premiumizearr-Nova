package utils

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
)

func StripDownloadTypesExtention(fileName string) string {
	var exts = [...]string{".nzb", ".magnet", ".torrent"}
	for _, ext := range exts {
		fileName = strings.TrimSuffix(fileName, ext)
	}

	return fileName
}

func StripMediaTypesExtention(fileName string) string {
	var exts = [...]string{".mkv", ".mp4", ".avi", ".mov", ".flv", ".wmv", ".mpg", ".mpeg", ".m4v", ".3gp", ".3g2", ".m2ts", ".mts", ".ts", ".webm", ".m4a", ".m4b", ".m4p", ".m4r", ".m4v"}
	for _, ext := range exts {
		fileName = strings.TrimSuffix(fileName, ext)
	}

	return fileName
}

// https://golangcode.com/unzip-files-in-go/
func Unzip(src string, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		// Store filename/path for returning and using later on
		fpath := filepath.Join(dest, f.Name)

		// Check for ZipSlip. More Info: https://snyk.io/research/zip-slip-vulnerability#go
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("%s: illegal file path", fpath)
		}

		if f.FileInfo().IsDir() {
			// Make Folder
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}

		// Make File
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		_, err = io.Copy(outFile, rc)

		// Close the file without defer to close before next iteration of loop
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

func StringInSlice(a string, list []string) int {
	for i, b := range list {
		if b == a {
			return i
		}
	}
	return -1
}

func GetDownloadsFolderIDFromPremiumizeme(premiumizemeClient *premiumizeme.Premiumizeme) string {
	var downloadsFolderID string
	folders, err := premiumizemeClient.GetFolders()
	if err != nil {
		log.Errorf("Error getting folders: %s", err)
		log.Errorf("Cannot read folders from premiumize.me, application will not run!")
		return ""
	}

	const folderName = "arrDownloads"

	for _, folder := range folders {
		if folder.Name == folderName {
			downloadsFolderID = folder.ID
			log.Debugf("Found downloads folder with ID: %s", folder.ID)
		}
	}

	if len(downloadsFolderID) == 0 {
		id, err := premiumizemeClient.CreateFolder(folderName, nil)
		if err != nil {
			log.Errorf("Cannot create downloads folder on premiumize.me, application will not run correctly! %+v", err)
		}
		downloadsFolderID = id
	}

	return downloadsFolderID
}

func EnvOrDefault(envName string, defaultValue string) string {
	envValue := os.Getenv(envName)
	if len(envValue) == 0 {
		return defaultValue
	}
	return envValue
}

func IsRunningInDockerContainer() bool {
	// docker creates a .dockerenv file at the root
	// of the directory tree inside the container.
	// if this file exists then the viewer is running
	// from inside a container so return true

	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	return false
}

func IsDirectoryWriteable(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Errorf("directory does not exist: ", path)
		return false
	}

	if _, err := os.Create(path + "/test.txt"); err != nil {
		log.Errorf("cannot write test.txt to directory: ", path)
		return false
	}

	// Delete test file
	if err := os.Remove(path + "/test.txt"); err != nil {
		log.Errorf("cannot delete test.txt file in: ", path)
		return false
	}

	return true
}

// https://stackoverflow.com/questions/33450980/how-to-remove-all-contents-of-a-directory-using-golang
func RemoveContents(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		err = os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
			return err
		}
	}
	return nil
}
