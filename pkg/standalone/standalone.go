package standalone

import (
	"archive/zip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	path_filepath "path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/actionscore/cli/pkg/print"
	"github.com/briandowns/spinner"
)

const baseDownloadURL = "https://actionsreleases.blob.core.windows.net/release"
const actionsImageURL = "actionscore.azurecr.io/actions"

func Init(runtimeVersion string) error {
	dir, err := getActionsDir()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	errorChan := make(chan error)

	initSteps := []func(*sync.WaitGroup, chan<- error, string, string){}
	initSteps = append(initSteps, installActionsBinary)
	initSteps = append(initSteps, runPlacementService)
	initSteps = append(initSteps, runRedis)

	wg.Add(len(initSteps))

	msg := "Downloading binaries and setting up components..."
	var s *spinner.Spinner
	if runtime.GOOS == "windows" {
		print.InfoStatusEvent(os.Stdout, msg)
	} else {
		s = spinner.New(spinner.CharSets[1], 100*time.Millisecond)
		s.Writer = os.Stdout
		s.Color("blue")
		s.Suffix = fmt.Sprintf("  %s", msg)
		s.Start()
	}

	for _, step := range initSteps {
		go step(&wg, errorChan, dir, runtimeVersion)
	}

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err := range errorChan {
		if err != nil {
			if s != nil {
				s.Stop()
			}
			return err
		}
	}

	if s != nil {
		s.Stop()
		print.SuccessStatusEvent(os.Stdout, msg)
	}

	return nil
}

func getActionsDir() (string, error) {
	p := ""

	if runtime.GOOS == "windows" {
		p = path_filepath.FromSlash("c:/actions")
	} else {
		usr, err := user.Current()
		if err != nil {
			return "", err
		}
		p = path.Join(usr.HomeDir, ".actions")
	}

	err := os.MkdirAll(p, 0700)
	if err != nil {
		return "", err
	}

	return p, nil
}

func runRedis(wg *sync.WaitGroup, errorChan chan<- error, dir, version string) {
	defer wg.Done()
	err := runCmd("docker", "run", "--restart", "always", "-d", "-p", "6379:6379", "redis")
	if err != nil {
		runError := isContainerRunError(err)
		if !runError {
			errorChan <- parseDockerError("Redis state store", err)
			return
		}
	}
	errorChan <- nil
}

func parseDockerError(component string, err error) error {
	if exitError, ok := err.(*exec.ExitError); ok {
		exitCode := exitError.ExitCode()
		if exitCode == 125 { //see https://github.com/moby/moby/pull/14012
			return fmt.Errorf("Failed to launch %s. Is it already running?", component)
		}
		if exitCode == 127 {
			return fmt.Errorf("Failed to launch %s. Make sure Docker is installed and running", component)
		}
	}
	return err
}

func isContainerRunError(err error) bool {
	if exitError, ok := err.(*exec.ExitError); ok {
		exitCode := exitError.ExitCode()
		return exitCode == 125
	}
	return false
}

func runPlacementService(wg *sync.WaitGroup, errorChan chan<- error, dir, version string) {
	defer wg.Done()

	osPort := 50005
	if runtime.GOOS == "windows" {
		osPort = 6050
	}

	image := fmt.Sprintf("%s:%s", actionsImageURL, version)
	err := runCmd("docker", "run", "--restart", "always", "-d", "-p", fmt.Sprintf("%v:50005", osPort), "--entrypoint", "./placement", image)
	if err != nil {
		runError := isContainerRunError(err)
		if !runError {
			errorChan <- parseDockerError("placement service", err)
			return
		}
	}
	errorChan <- nil
}

func installActionsBinary(wg *sync.WaitGroup, errorChan chan<- error, dir, version string) {
	defer wg.Done()

	actionsURL := fmt.Sprintf("%s/%s/actionsrt_%s_%s.zip", baseDownloadURL, version, runtime.GOOS, runtime.GOARCH)
	filepath, err := downloadFile(dir, actionsURL)
	if err != nil {
		errorChan <- fmt.Errorf("Error downloading actions binary: %s", err)
		return
	}

	extractedFilePath, err := extractFile(filepath, dir)
	if err != nil {
		errorChan <- fmt.Errorf("Error extracting actions binary: %s", err)
		return
	}

	actionsPath, err := moveFileToPath(extractedFilePath)
	if err != nil {
		errorChan <- fmt.Errorf("Error moving actions binary to path: %s", err)
		return
	}

	err = makeExecutable(actionsPath)
	if err != nil {
		errorChan <- fmt.Errorf("Error making actions binary executable: %s", err)
		return
	}

	errorChan <- nil
}

func makeExecutable(filepath string) error {
	if runtime.GOOS != "windows" {
		err := os.Chmod(filepath, 0777)
		if err != nil {
			return err
		}
	}

	return nil
}

func runCmd(name string, arg ...string) error {
	cmd := exec.Command(name, arg...)
	err := cmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func extractFile(filepath, targetDir string) (string, error) {
	zipReader, err := zip.OpenReader(filepath)
	if err != nil {
		return "", err
	}

	for _, file := range zipReader.Reader.File {
		zippedFile, err := file.Open()
		if err != nil {
			return "", err
		}
		defer zippedFile.Close()

		extractedFilePath := path.Join(
			targetDir,
			file.Name,
		)

		outputFile, err := os.OpenFile(
			extractedFilePath,
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
			file.Mode(),
		)
		if err != nil {
			return "", err
		}
		defer outputFile.Close()

		_, err = io.Copy(outputFile, zippedFile)
		if err != nil {
			return "", err
		}

		return extractedFilePath, nil
	}

	return "", nil
}

func moveFileToPath(filepath string) (string, error) {
	fileName := path_filepath.Base(filepath)
	destFilePath := ""

	if runtime.GOOS == "windows" {
		p := os.Getenv("PATH")
		if !strings.Contains(strings.ToLower(string(p)), strings.ToLower("c:\\actions")) {
			err := runCmd("SETX", "PATH", p+";c:\\actions")
			if err != nil {
				return "", err
			}
		}
		return "c:\\actions\\actionsrt.exe", nil
	}

	destFilePath = path.Join("/usr/local/bin", fileName)

	input, err := ioutil.ReadFile(filepath)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(destFilePath, input, 0644)
	if err != nil {
		return "", err
	}

	return destFilePath, nil
}

func downloadFile(dir string, url string) (string, error) {
	tokens := strings.Split(url, "/")
	fileName := tokens[len(tokens)-1]

	filepath := path.Join(dir, fileName)
	_, err := os.Stat(filepath)
	if os.IsExist(err) {
		return "", nil
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", err
	}

	return filepath, nil
}
