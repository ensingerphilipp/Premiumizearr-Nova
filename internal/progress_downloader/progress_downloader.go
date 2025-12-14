package progress_downloader

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"

	"github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"
)

// WriteCounter tracks download progress.
type WriteCounter struct {
	TotalDownloaded uint64 // Total bytes downloaded
	Percentage      string // Percentage completed (e.g., "1%")
	Speed           string // Download speed (e.g., "35.4M")
	RemainingTime   string // Time remaining (e.g., "18m41s")
}

// NewWriteCounter creates a new WriteCounter.
func NewWriteCounter() *WriteCounter {
	return &WriteCounter{
		TotalDownloaded: 0,
		Percentage:      "0%",
		Speed:           "0",
		RemainingTime:   "unknown",
	}
}

// GetSpeed returns the current download speed as a string.
func (wc *WriteCounter) GetSpeed() string {
	return fmt.Sprintf("%sB / Second", wc.Speed)
}

// GetProgress returns the progress as a human-readable string.
func (wc *WriteCounter) GetProgress() string {
	//log.Infof("GetProgress called: %s complete", humanize.Bytes(wc.TotalDownloaded))
	return fmt.Sprintf("%s Complete (%s)", wc.Percentage, humanize.Bytes(wc.TotalDownloaded))
}

// DownloadFile uses wget for downloading and updates WriteCounter for progress tracking.
func DownloadFile(checkcertificate bool, ratelimit int, url string, filepath string, counter *WriteCounter) error {
	// Prepare the wget command without passing empty args
	args := []string{"-oL", "wget", "-c"}
	if ratelimit != 0 {
		args = append(args, "--limit-rate="+strconv.Itoa(ratelimit)+"M")
	}
	args = append(args, "--progress=dot:giga")
	if checkcertificate == false {
		args = append(args, "--no-check-certificate")
	}
	args = append(args, "--no-use-server-timestamps", "-O", filepath, url)
	cmd := exec.Command("stdbuf", args...)

	// Get a pipe for the command's output
	stdout, err := cmd.StderrPipe()
	if err != nil {
		log.Errorf("failed to create stdout pipe: %v", err)
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		log.Errorf("failed to start wget: %v", err)
		return fmt.Errorf("failed to start wget: %v", err)
	}
	// Regex to parse output lines
	progressRegex := regexp.MustCompile(`(\d+)([KMG]?) .* (\d+%) (\d+\.\d+[KMG]) (\d+[smh][0-9]*[smh]?)`)
	// Scan the output
	scanner := bufio.NewScanner(stdout)
	//log.Infof("Scanner created")
	for scanner.Scan() {
		line := scanner.Text()
		// Check if the line matches the regex
		matches := progressRegex.FindStringSubmatch(line)
		if len(matches) == 6 { // Full match expected
			// Extract values
			sizeStr, unit, percentage, speed, remainingTime := matches[1], matches[2], matches[3], matches[4], matches[5]
			// Convert size to bytes
			sizeInBytes, _ := strconv.ParseUint(sizeStr, 10, 64)
			switch unit {
			case "K":
				sizeInBytes *= 1024
			case "M":
				sizeInBytes *= 1024 * 1024
			case "G":
				sizeInBytes *= 1024 * 1024 * 1024
			}

			// Update the counter with parsed values
			counter.TotalDownloaded = sizeInBytes
			counter.Percentage = percentage
			counter.Speed = speed
			counter.RemainingTime = remainingTime

			// Print progress
			//fmt.Printf("\r%s complete (%s, %s remaining)", counter.GetProgress(), counter.GetSpeed(), counter.RemainingTime)
			//log.Infof("\r%s complete (%s, %s remaining)", counter.GetProgress(), counter.GetSpeed(), counter.RemainingTime)
		}
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		log.Errorf("error reading wget output: %v", err)
		return fmt.Errorf("error reading wget output: %v", err)
	}

	// Wait for wget to finish
	if err := cmd.Wait(); err != nil {
		log.Errorf("wget command failed: %v", err)
		return fmt.Errorf("wget command failed: %v", err)
	}

	fmt.Println("\nDownload completed successfully.")
	return nil
}
