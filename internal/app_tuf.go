package internal

import (
	"fmt"
	"log"
	stdlog "log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/detsch/go-tuf/v2/metadata"
	"github.com/detsch/go-tuf/v2/metadata/config"
	"github.com/detsch/go-tuf/v2/metadata/updater"
	"github.com/go-logr/stdr"
)

var (
	globalApp *App
)

type FioFetcher struct {
	client  *http.Client
	tag     string
	repoUrl string
}

func readLocalFile(filePath string) ([]byte, error) {
	fmt.Println("Read local file: " + filePath)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: 404, URL: "file://" + filePath}
	}
	return data, nil
}

func readRemoteFile(d *FioFetcher, urlPath string, maxLength int64) ([]byte, error) {
	fmt.Println("Read remote file: " + urlPath)
	headers := make(map[string]string)
	fmt.Println("Setting x-ats-tags=" + d.tag)
	headers["x-ats-tags"] = d.tag
	res, err := httpGet(d.client, urlPath, headers)

	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, &metadata.ErrDownloadHTTP{StatusCode: res.StatusCode, URL: urlPath}
	}

	fmt.Println("GET RESULT=" + string(res.Body))

	var length int64
	// Get content length from header (might not be accurate, -1 or not set).
	if header := res.Header.Get("Content-Length"); header != "" {
		length, err = strconv.ParseInt(header, 10, 0)
		if err != nil {
			return nil, err
		}
		// Error if the reported size is greater than what is expected.
		if length > maxLength {
			return nil, &metadata.ErrDownloadLengthMismatch{Msg: fmt.Sprintf("download failed for %s, length %d is larger than expected %d", urlPath, length, maxLength)}
		}
	}
	// // Although the size has been checked above, use a LimitReader in case
	// // the reported size is inaccurate, or size is -1 which indicates an
	// // unknown length. We read maxLength + 1 in order to check if the read data
	// // surpased our set limit.
	// data, err := io.ReadAll(io.LimitReader(res.Body, maxLength+1))
	// if err != nil {
	// 	return nil, err
	// }
	// Error if the reported size is greater than what is expected.
	length = int64(len(res.Body))
	if length > maxLength {
		return nil, &metadata.ErrDownloadLengthMismatch{Msg: fmt.Sprintf("download failed for %s, length %d is larger than expected %d", urlPath, length, maxLength)}
	}

	return res.Body, nil
}

// DownloadFile downloads a file from urlPath, errors out if it failed,
// its length is larger than maxLength or the timeout is reached.
func (d *FioFetcher) DownloadFile(urlPath string, maxLength int64, timeout time.Duration) ([]byte, error) {
	if strings.HasPrefix(urlPath, "file://") {
		return readLocalFile(urlPath[len("file://"):])
	} else {
		return readRemoteFile(d, urlPath, maxLength)
	}
}

func getTufCfg(client *http.Client, repoUrl string, tag string) (*config.UpdaterConfig, error) {
	// TODO: do not hardcode path:
	localMetadataDir := "/var/sota/tuf/"
	rootBytes, err := os.ReadFile(filepath.Join(localMetadataDir, "root.json"))
	if err != nil {
		log.Println("os.ReadFile error")
		return nil, err
	}

	// create updater configuration
	cfg, err := config.New(repoUrl, rootBytes) // default config
	if err != nil {
		log.Println("config.New(repoUrl, error")
		return nil, err
	}
	cfg.LocalMetadataDir = localMetadataDir
	cfg.LocalTargetsDir = filepath.Join(localMetadataDir, "download")
	cfg.RemoteTargetsURL = repoUrl
	cfg.PrefixTargetsWithHash = true
	cfg.Fetcher = &FioFetcher{
		client:  client,
		tag:     tag,
		repoUrl: repoUrl,
	}
	return cfg, nil
}

var (
	fioUpdater *updater.Updater
	fioClient  *http.Client
)

func refreshTuf(client *http.Client, repoUrl string, tag string) error {
	cfg, err := getTufCfg(client, repoUrl, tag)
	if err != nil {
		log.Println("failed to create Config instance: %w", err)
		return err
	}

	// create a new Updater instance
	up, err := updater.New(cfg)
	if err != nil {
		log.Println("failed to create Updater instance: %w", err)
		return err
	}

	// fioUpdater is used to read the current targets data
	fioUpdater = up

	// try to build the top-level metadata
	err = up.Refresh()
	if err != nil {
		log.Println("failed to refresh trusted metadata: %w", err)
		return err
	}
	for name := range up.GetTopLevelTargets() {
		log.Println("target name " + name)
	}
	return nil
}

func (a *App) refreshTufApp(client *http.Client, localRepoPath string) error {
	metadata.SetLogger(stdr.New(stdlog.New(os.Stdout, "fioconfig", stdlog.LstdFlags)))
	fioClient = client
	var repoUrl string
	tag := a.sota.Get("pacman.tags")
	fmt.Println("refreshTufApp tag=" + tag)
	if localRepoPath == "" {
		repoUrl = strings.Replace(a.configUrl, "/config", "/repo", -1)
	} else {
		if strings.HasPrefix(localRepoPath, "file://") {
			repoUrl = localRepoPath
		} else {
			repoUrl = "file://" + localRepoPath
		}
	}
	ret := refreshTuf(client, repoUrl, tag)
	return ret
}

func DieNotNil(err error, message ...string) {
	if err != nil {
		parts := []interface{}{"ERROR:"}
		for _, p := range message {
			parts = append(parts, p)
		}
		parts = append(parts, err)
		fmt.Println(parts...)
		os.Exit(1)
	}
}

func getTargetsHttp(c *gin.Context) {
	ret := []string{}
	targets := fioUpdater.GetTopLevelTargets()
	for name, _ := range targets {
		t, _ := targets[name].MarshalJSON()
		ret = append(ret, string(t))
	}

	c.IndentedJSON(http.StatusOK, targets)
}

func getRootHttp(c *gin.Context) {
	c.IndentedJSON(http.StatusOK, fioUpdater.GetTrustedMetadataSet().Root)
}

type tufError struct {
	s string
}

func (f tufError) Error() string {
	return "TUF error"
}

func refreshTufHttp(c *gin.Context) {
	log.Println("UpdateTargets BEGIN localTufRepo=" + c.Query("localTufRepo"))
	err := globalApp.refreshTufApp(fioClient, c.Query("localTufRepo"))
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, tufError{fmt.Sprintf("failed to create Config instance: %w", err)})
	}
	c.Done()
	log.Println("UpdateTargets END")
}

func startHttpServer() {
	router := gin.Default()
	router.GET("/targets", getTargetsHttp)
	router.GET("/root", getRootHttp)
	router.POST("/targets/update/", refreshTufHttp)
	fmt.Println("Starting test http server at port 9080")
	router.Run("localhost:9080")
	fmt.Println("Exit from port 9080")
}

func (a *App) StartTufAgent() error {
	globalApp = a
	client, crypto := createClient(a.sota)
	// defer crypto.Close()
	a.callInitFunctions(client, crypto)

	a.refreshTufApp(client, "")

	startHttpServer()
	return nil
}