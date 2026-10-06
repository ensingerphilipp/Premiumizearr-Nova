package directclient

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/progress_downloader"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// publishedManifestName is the marker file the atomic publish writes into
// the published tree, BEFORE the staging rename, tying the published
// directory to the downloader that owns it.
const publishedManifestName = ".directclient-published"

// DownloadCloudFolder copies a Premiumize cloud folder into outputPath. Files
// are downloaded into a sibling staging directory and only exposed at
// outputPath once every file has completed. Successfully downloaded files in
// the staging directory are retained across retries; wget's own partial file
// is retained as well so it can resume an interrupted transfer.
//
// publishedKey identifies the downloader that owns the published output
// (the manager passes the job ID). The publish writes a manifest carrying
// that key into the tree, and a re-invocation treats an existing output
// directory as a finished publish ONLY when its manifest matches the key.
//
// Progress reports bytes already present in the staging directory after each
// completed file. Premiumize's folder listing currently has no size field, so
// total is zero (unknown).
func DownloadCloudFolder(ctx context.Context, pm *premiumizeme.Premiumizeme, folderID, outputPath, publishedKey string, tlsCheck bool, speedLimit int, progress func(done, total int64)) error {
	if pm == nil {
		return fmt.Errorf("premiumize client is nil")
	}
	if strings.TrimSpace(folderID) == "" {
		return fmt.Errorf("premiumize folder ID is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return fmt.Errorf("output path is empty")
	}
	if strings.TrimSpace(publishedKey) == "" {
		return fmt.Errorf("published key is empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	finalPath, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if info, err := os.Lstat(finalPath); err == nil {
		if info.IsDir() {
			// "The directory exists" is not verifiable as "I published
			// it": a pre-created or re-used directory may hold foreign
			// content, and treating it as success would let the caller
			// delete the cloud source folder above content this call
			// never delivered. Only the manifest written by the atomic
			// publish below, keyed to this downloader, proves it.
			if data, rerr := os.ReadFile(filepath.Join(finalPath, publishedManifestName)); rerr == nil &&
				strings.TrimSpace(string(data)) == publishedKey {
				// A prior call may have finished the atomic publish and
				// lost its response. Treat it as success to make
				// retries idempotent.
				return nil
			}
			return fmt.Errorf("output path %s exists but was not published by this downloader (key %q) for folder %s", finalPath, publishedKey, folderID)
		}
		return fmt.Errorf("output path already exists and is not a directory")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect output path: %w", err)
	}

	parent := filepath.Dir(finalPath)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("create output parent: %w", err)
	}
	stagePath := finalPath + ".partial"
	if err := ensureRealDirectory(stagePath); err != nil {
		return fmt.Errorf("prepare staging directory: %w", err)
	}

	var files []cloudFile
	visited := make(map[string]bool)
	if err := collectCloudFiles(ctx, pm, folderID, "", visited, &files); err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("Premiumize folder is empty")
	}
	seen := make(map[string]struct{}, len(files))
	var total int64
	for _, file := range files {
		if _, ok := seen[file.relativePath]; ok {
			return fmt.Errorf("premiumize folder contains duplicate path %q", file.relativePath)
		}
		seen[file.relativePath] = struct{}{}
		if file.size > 0 {
			total += file.size
		}
	}
	// An entry whose name is another entry's in-progress staging name
	// ("<name>.<id>.partial") would make one file's wget resume against the
	// other file's completed bytes: fail closed on the class, before any
	// download starts.
	targets := make(map[string]struct{}, len(files))
	for _, file := range files {
		targets[filepath.Join(stagePath, file.relativePath)] = struct{}{}
	}
	for _, file := range files {
		inProgress := fmt.Sprintf("%s.%s.partial", filepath.Join(stagePath, file.relativePath), file.id)
		if _, ok := targets[inProgress]; ok {
			return fmt.Errorf("premiumize folder entry %q collides with another file's in-progress name; refusing to mix content", file.relativePath)
		}
	}

	var done int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(stagePath, file.relativePath)
		if err := ensureSafeParent(stagePath, filepath.Dir(target)); err != nil {
			return fmt.Errorf("prepare file %q: %w", file.relativePath, err)
		}
		if info, err := os.Lstat(target); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("staged path %q is not a regular file", file.relativePath)
			}
			// A staged target is trusted only when its completion sidecar
			// names THIS entry's file ID. A file that merely exists at the
			// path may be a stale leftover from an earlier listing (a
			// replaced entry, a renamed sibling), and trusting it would
			// publish the old bytes under the new name; instead the fresh
			// download replaces it.
			if owner, rerr := os.ReadFile(completeSidecar(target)); rerr == nil &&
				strings.TrimSpace(string(owner)) == file.id {
				done += info.Size()
				if progress != nil {
					progress(done, total)
				}
				continue
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect staged file %q: %w", file.relativePath, err)
		}

		link, err := pm.GenerateFileLink(file.id)
		if err != nil {
			return fmt.Errorf("generate Premiumize link for %q: %w", file.relativePath, err)
		}
		// The in-progress name must be unique per file: a cloud entry whose
		// name is a sibling file's name plus ".partial" would otherwise make
		// wget -c resume against the sibling's completed content. The
		// Premiumize file ID is stable across restarts, so a legitimately
		// interrupted transfer still resumes, and no listed name can match
		// <name>.<opaque-id>.partial short of self-reference.
		partialPath := fmt.Sprintf("%s.%s.partial", target, file.id)
		monitorStop := make(chan struct{})
		if progress != nil && total > 0 {
			go func(base int64) {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if info, err := os.Stat(partialPath); err == nil {
							progress(base+info.Size(), total)
						}
					case <-monitorStop:
						return
					}
				}
			}(done)
		}
		downloadErr := progress_downloader.DownloadFileContext(ctx, tlsCheck, speedLimit, link, partialPath, progress_downloader.NewWriteCounter())
		close(monitorStop)
		if err := downloadErr; err != nil {
			// The progress downloader deletes malformed/failed partial output
			// in some failure cases. Preserve whatever remains for retry.
			return fmt.Errorf("download Premiumize file %q: %w", file.relativePath, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Rename(partialPath, target); err != nil {
			return fmt.Errorf("finish staged file %q: %w", file.relativePath, err)
		}
		// Bind the staged target to the listing entry that produced it:
		// the sidecar is consulted by the skip above, so a retry re-uses
		// a completed download of the SAME entry instead of re-downloading
		// it, and re-downloads any target a previous listing left behind.
		if err := os.WriteFile(completeSidecar(target), []byte(file.id), 0644); err != nil {
			return fmt.Errorf("record completion of staged file %q: %w", file.relativePath, err)
		}
		info, err := os.Stat(target)
		if err != nil {
			return fmt.Errorf("stat staged file %q: %w", file.relativePath, err)
		}
		done += info.Size()
		if progress != nil {
			progress(done, total)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	// The publish renames the WHOLE staging directory into the output
	// tree, so first delete every staged entry no entry of the
	// just-collected listing claims: an earlier listing may have left
	// behind in-progress names and stale targets that belong to no file
	// in this folder, and shipping them would make *arr import foreign
	// bytes as part of the release.
	if err := pruneUnclaimedStaging(stagePath, files); err != nil {
		return err
	}
	// Tie the published tree to its downloader BEFORE the atomic publish:
	// on a later retry the manifest is the only proof that the existing
	// output directory was published by this downloader.
	if err := os.WriteFile(filepath.Join(stagePath, publishedManifestName), []byte(publishedKey), 0644); err != nil {
		return fmt.Errorf("record published folder: %w", err)
	}
	if err := os.Rename(stagePath, finalPath); err != nil {
		return fmt.Errorf("publish downloaded folder: %w", err)
	}
	return nil
}

// completeSidecar is the per-file completion marker inside the staging
// tree; its content records the listing entry ID that produced the staged
// target, binding the target to the entry that owns it.
func completeSidecar(target string) string { return target + ".complete" }

// pruneUnclaimedStaging deletes every staged entry that no entry of the
// current cloud listing claims: a directory is claimed only when a
// claimed entry lies inside it. It runs before the manifest write and the
// staging rename, because the publish moves the WHOLE staging tree into
// the output path — only claimed entries may survive.
func pruneUnclaimedStaging(stagePath string, files []cloudFile) error {
	// Claimed entries as paths relative to the staging root.
	claimedFiles := make(map[string]struct{}, 3*len(files)+1)
	for _, file := range files {
		claimedFiles[file.relativePath] = struct{}{}
		claimedFiles[file.relativePath+"."+file.id+".partial"] = struct{}{}
		claimedFiles[completeSidecar(file.relativePath)] = struct{}{}
	}
	claimedFiles[publishedManifestName] = struct{}{}
	claimedDirs := make(map[string]struct{})
	for rel := range claimedFiles {
		dir := filepath.Dir(rel)
		for dir != "." {
			claimedDirs[dir] = struct{}{}
			dir = filepath.Dir(dir)
		}
	}
	return filepath.WalkDir(stagePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(stagePath, p)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if _, ok := claimedDirs[rel]; !ok {
				if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
					return err
				}
				return fs.SkipDir
			}
			return nil
		}
		if _, ok := claimedFiles[rel]; !ok {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}

type cloudFile struct {
	id           string
	relativePath string
	size         int64
}

func collectCloudFiles(ctx context.Context, pm *premiumizeme.Premiumizeme, folderID, parent string, visited map[string]bool, files *[]cloudFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visited[folderID] {
		return fmt.Errorf("Premiumize folder tree contains a cycle at %q", folderID)
	}
	visited[folderID] = true
	items, err := pm.ListFolderContext(ctx, folderID)
	if err != nil {
		return fmt.Errorf("list Premiumize folder: %w", err)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := safeEntryName(item.Name)
		if err != nil {
			return err
		}
		// The manifest is written into the staging root for a file entry
		// (which would truncate this file to the manifest key before
		// publish) and collides with the root for a folder entry (EISDIR
		// at the manifest write), so a listed entry with that name cannot
		// be represented. Reject both before any download starts.
		if name == publishedManifestName {
			return fmt.Errorf("premiumize entry %q uses the downloader's reserved manifest name %q; refusing to download a listing the publish step cannot represent", parent+"/"+name, publishedManifestName)
		}
		rel := filepath.Join(parent, name)
		switch strings.ToLower(item.Type) {
		case "folder":
			if item.ID == "" {
				return fmt.Errorf("Premiumize folder %q has no ID", name)
			}
			if err := collectCloudFiles(ctx, pm, item.ID, rel, visited, files); err != nil {
				return err
			}
		case "file":
			if item.ID == "" {
				return fmt.Errorf("Premiumize file %q has no ID", rel)
			}
			*files = append(*files, cloudFile{id: item.ID, relativePath: rel, size: item.Size})
		default:
			return fmt.Errorf("Premiumize item %q has unsupported type %q", name, item.Type)
		}
	}
	delete(visited, folderID)
	return nil
}

func safeEntryName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("unsafe Premiumize item name %q", name)
	}
	return name, nil
}

func ensureRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("staging path is not a real directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.Mkdir(path, 0755)
}

func ensureSafeParent(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes staging directory")
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("staging parent is not a real directory")
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(current, 0755); err != nil {
			return err
		}
	}
	return nil
}
