/*
 Copyright 2020 Qiniu Cloud (qiniu.com)

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cover

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/tools/cover"
	"k8s.io/test-infra/gopherage/pkg/cov"
)

// LogFile a file to save log.
const LogFile = "goc.log"

type server struct {
	PersistenceFile string
	IPRevise        bool // whether to do ip revise during registering
	Store           Store
}

// NewFileBasedServer new a file based server with persistenceFile
func NewFileBasedServer(persistenceFile string) (*server, error) {
	store, err := NewFileStore(persistenceFile)
	if err != nil {
		return nil, err
	}
	return &server{
		PersistenceFile: persistenceFile,
		Store:           store,
	}, nil
}

// NewMemoryBasedServer new a memory based server without persistenceFile
func NewMemoryBasedServer() *server {
	return &server{
		Store: NewMemoryStore(),
	}
}

// Run starts coverage host center
func (s *server) Run(port string) {
	f, err := os.Create(LogFile)
	if err != nil {
		log.Fatalf("failed to create log file %s, err: %v", LogFile, err)
	}

	// both log to stdout and file by default
	mw := io.MultiWriter(f, os.Stdout)
	r := s.Route(mw)
	log.Fatal(r.Run(port))
}

// Router init goc server engine
func (s *server) Route(w io.Writer) *gin.Engine {
	if w != nil {
		gin.DefaultWriter = w
	}
	r := gin.Default()
	// api to show the registered services
	r.StaticFile("static", "./"+s.PersistenceFile)

	v1 := r.Group("/v1")
	{
		v1.POST("/cover/register", s.registerService)
		v1.GET("/cover/profile/", s.profileByServiceName)
		v1.GET("/cover/profile", s.profile)
		v1.POST("/cover/profile", s.profile)
		v1.POST("/cover/clear", s.clear)
		v1.POST("/cover/init", s.initSystem)
		v1.GET("/cover/list", s.listServices)
		v1.POST("/cover/remove", s.removeServices)
	}

	return r
}

// ServiceUnderTest is a entry under being tested
type ServiceUnderTest struct {
	Name     string `form:"name" json:"name" binding:"required"`
	Address  string `form:"address" json:"address" binding:"required"`
	IPRevise string `form:"ip_revise" json:"ip_revise" binding:"-"` // whether to do ip revise during registering
}

// ProfileParam is param of profile API
type ProfileParam struct {
	Force             bool     `form:"force" json:"force"`
	Service           []string `form:"service" json:"service"`
	Address           []string `form:"address" json:"address"`
	CoverFilePatterns []string `form:"coverfile" json:"coverfile"`
	SkipFilePatterns  []string `form:"skipfile" json:"skipfile"`
}

// listServices list all the registered services
func (s *server) listServices(c *gin.Context) {
	services := s.Store.GetAll()
	c.JSON(http.StatusOK, services)
}

func (s *server) registerService(c *gin.Context) {
	var service ServiceUnderTest
	if err := c.ShouldBind(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, err := url.Parse(service.Address)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("url.Parse %s failed: %s", service.Address, err.Error())})
		return
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupport schema"})
		return
	}
	if u.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty host"})
		return
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		if strings.Contains(err.Error(), "missing port in address") {
			// valid scenario, keep going
			host = u.Host
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("net.SplitHostPort %s failed: %s", u.Host, err.Error())})
			return
		}
	}

	var doIPRevise bool
	// Prefer user's decision first.
	if service.IPRevise != "" {
		doIPRevise, err = strconv.ParseBool(service.IPRevise)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("strconv.ParseBool %s failed: %s", service.IPRevise, err.Error())})
			return
		}
	} else {
		doIPRevise = s.IPRevise
	}

	if doIPRevise {
		realIP := c.ClientIP()
		// only for IPV4
		// refer: https://github.com/qiniu/goc/issues/177
		if net.ParseIP(realIP).To4() != nil && host != realIP {
			log.Printf("the registered host %s of service %s is different with the real one %s, here we choose the real one", service.Name, host, realIP)
			host = realIP
		}
	}

	service.Address = fmt.Sprintf("%s://%s", u.Scheme, host)
	if port != "" {
		service.Address = fmt.Sprintf("%s:%s", service.Address, port)
	}

	address := s.Store.Get(service.Name)
	if !contains(address, service.Address) {
		if err := s.Store.Add(service); err != nil && err != ErrServiceAlreadyRegistered {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"result": "success"})
}

// profile API examples:
// POST /v1/cover/profile
// { "force": "true", "service":["a","b"], "address":["c","d"],"coverfile":["e","f"] }
func (s *server) profile(c *gin.Context) {
	var body ProfileParam
	if err := c.ShouldBind(&body); err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}

	allInfos := s.Store.GetAll()
	filterAddrInfoList, err := filterAddrInfo(body.Service, body.Address, body.Force, allInfos)
	if err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}

	var mergedProfiles = make([][]*cover.Profile, 0)
	for _, addrInfo := range filterAddrInfoList {
		pp, err := NewWorker(addrInfo.Address).Profile(ProfileParam{})
		if err != nil {
			if body.Force {
				log.Warnf("get profile from [%s] failed, error: %s", addrInfo, err.Error())
				continue
			}

			c.JSON(http.StatusExpectationFailed, gin.H{"error": fmt.Sprintf("failed to get profile from %s, service %s, error %s", addrInfo.Address, addrInfo.Name, err.Error())})
			return
		}

		profile, err := convertProfile(pp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		mergedProfiles = append(mergedProfiles, profile)
	}

	if len(mergedProfiles) == 0 {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": "no profiles"})
		return
	}

	merged, err := cov.MergeMultipleProfiles(mergedProfiles)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(body.CoverFilePatterns) > 0 {
		merged, err = filterProfile(body.CoverFilePatterns, merged)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to filter profile based on the patterns: %v, error: %v", body.CoverFilePatterns, err)})
			return
		}
	}

	if len(body.SkipFilePatterns) > 0 {
		merged, err = skipProfile(body.SkipFilePatterns, merged)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to skip profile based on the patterns: %v, error: %v", body.SkipFilePatterns, err)})
			return
		}
	}

	if err := cov.DumpProfile(merged, c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

// profileByServiceName API examples:
// GET /v1/cover/profile/?name=relay-agent (returns raw coverage profile)
// GET /v1/cover/profile/?name=relay-agent&format=html (returns HTML coverage report)
func (s *server) profileByServiceName(c *gin.Context) {
	serviceName := c.Query("name")
	if serviceName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "service name is required"})
		return
	}

	allInfos := s.Store.GetAll()
	serviceAddresses, exists := allInfos[serviceName]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("service [%s] not found", serviceName)})
		return
	}

	if len(serviceAddresses) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("service [%s] has no registered instances", serviceName)})
		return
	}

	var mergedProfiles = make([][]*cover.Profile, 0)
	var failedAddresses []string

	for _, address := range serviceAddresses {
		pp, err := NewWorker(address).Profile(ProfileParam{})
		if err != nil {
			log.Warnf("get profile from [%s] failed, error: %s", address, err.Error())
			failedAddresses = append(failedAddresses, address)
			continue
		}

		profile, err := convertProfile(pp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		mergedProfiles = append(mergedProfiles, profile)
	}

	if len(mergedProfiles) == 0 {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": fmt.Sprintf("no profiles available for service [%s]. Failed addresses: %v", serviceName, failedAddresses)})
		return
	}

	merged, err := cov.MergeMultipleProfiles(mergedProfiles)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Check if HTML format is requested
	format := c.Query("format")
	if format == "html" {
		htmlOutput, err := convertCoverageToHTML(merged)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to convert coverage to HTML: %v", err)})
			return
		}
		c.Header("Content-Type", "text/html")
		c.String(http.StatusOK, string(htmlOutput))
		return
	}

	if err := cov.DumpProfile(merged, c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

// convertCoverageToHTML converts coverage profile to HTML using embedded HTML generation
func convertCoverageToHTML(profiles []*cover.Profile) ([]byte, error) {
	var buf bytes.Buffer

	// Generate HTML coverage report
	if err := generateHTMLCoverageReport(profiles, &buf); err != nil {
		return nil, fmt.Errorf("failed to generate HTML coverage report: %v", err)
	}

	return buf.Bytes(), nil
}

// generateHTMLCoverageReport generates an HTML coverage report from coverage profiles
func generateHTMLCoverageReport(profiles []*cover.Profile, w io.Writer) error {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Coverage Report</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; }
        h1 { color: #333; }
        .summary { background: #f5f5f5; padding: 15px; margin: 20px 0; border-radius: 5px; }
        .file { margin: 20px 0; border: 1px solid #ddd; border-radius: 5px; }
        .file-header { background: #f9f9f9; padding: 10px; font-weight: bold; border-bottom: 1px solid #ddd; }
        .coverage-high { background-color: #d4edda; }
        .coverage-medium { background-color: #fff3cd; }
        .coverage-low { background-color: #f8d7da; }
        .coverage-none { background-color: #f5f5f5; }
        .block { margin: 5px 0; padding: 5px; border-radius: 3px; }
        .covered { background-color: #c3e6cb; }
        .not-covered { background-color: #f5c6cb; }
        table { width: 100%; border-collapse: collapse; margin: 10px 0; }
        th, td { padding: 8px; text-align: left; border-bottom: 1px solid #ddd; }
        th { background-color: #f2f2f2; }
        .percent { text-align: right; }
    </style>
</head>
<body>
    <h1>Coverage Report</h1>
    
    <div class="summary">
        <h2>Summary</h2>
        <table>
            <tr><th>File</th><th>Coverage</th><th>Lines</th><th>Covered</th></tr>
            {{range .Files}}
            <tr>
                <td>{{.Name}}</td>
                <td class="percent">{{printf "%.1f%%" .Coverage}}</td>
                <td class="percent">{{.TotalLines}}</td>
                <td class="percent">{{.CoveredLines}}</td>
            </tr>
            {{end}}
        </table>
        <p><strong>Total Coverage: {{printf "%.1f%%" .TotalCoverage}}</strong></p>
    </div>

    {{range .Files}}
    <div class="file">
        <div class="file-header">{{.Name}} ({{printf "%.1f%%" .Coverage}})</div>
        {{range .Blocks}}
        <div class="block {{if .Covered}}covered{{else}}not-covered{{end}}">
            Lines {{.StartLine}}-{{.EndLine}}: {{if .Covered}}✓ Covered ({{.Count}} times){{else}}✗ Not covered{{end}}
        </div>
        {{end}}
    </div>
    {{end}}
</body>
</html>`

	// Calculate coverage statistics
	data := calculateCoverageStats(profiles)

	// Parse and execute template
	t, err := template.New("coverage").Parse(tmpl)
	if err != nil {
		return fmt.Errorf("failed to parse template: %v", err)
	}

	return t.Execute(w, data)
}

// CoverageData represents the data structure for the HTML template
type CoverageData struct {
	TotalCoverage float64
	Files         []FileData
}

type FileData struct {
	Name         string
	Coverage     float64
	TotalLines   int
	CoveredLines int
	Blocks       []BlockData
}

type BlockData struct {
	StartLine int
	EndLine   int
	Covered   bool
	Count     int
}

// calculateCoverageStats calculates coverage statistics from profiles
func calculateCoverageStats(profiles []*cover.Profile) CoverageData {
	var data CoverageData
	var totalStatements, coveredStatements int64

	for _, profile := range profiles {
		fileData := FileData{
			Name:   profile.FileName,
			Blocks: make([]BlockData, 0),
		}

		var fileStatements, fileCovered int64

		for _, block := range profile.Blocks {
			blockData := BlockData{
				StartLine: block.StartLine,
				EndLine:   block.EndLine,
				Covered:   block.Count > 0,
				Count:     block.Count,
			}
			fileData.Blocks = append(fileData.Blocks, blockData)

			statements := int64(block.NumStmt)
			fileStatements += statements
			totalStatements += statements

			if block.Count > 0 {
				fileCovered += statements
				coveredStatements += statements
			}
		}

		fileData.TotalLines = int(fileStatements)
		fileData.CoveredLines = int(fileCovered)
		if fileStatements > 0 {
			fileData.Coverage = float64(fileCovered) / float64(fileStatements) * 100
		}

		data.Files = append(data.Files, fileData)
	}

	if totalStatements > 0 {
		data.TotalCoverage = float64(coveredStatements) / float64(totalStatements) * 100
	}

	return data
}

// filterProfile filters profiles of the packages matching the coverFile pattern
func filterProfile(coverFile []string, profiles []*cover.Profile) ([]*cover.Profile, error) {
	var out = make([]*cover.Profile, 0)
	for _, profile := range profiles {
		for _, pattern := range coverFile {
			matched, err := regexp.MatchString(pattern, profile.FileName)
			if err != nil {
				return nil, fmt.Errorf("filterProfile failed with pattern %s for profile %s, err: %v", pattern, profile.FileName, err)
			}
			if matched {
				out = append(out, profile)
				break // no need to check again for the file
			}
		}
	}

	return out, nil
}

// skipProfile skips profiles of the packages matching the skipFile pattern
func skipProfile(skipFile []string, profiles []*cover.Profile) ([]*cover.Profile, error) {
	var out = make([]*cover.Profile, 0)
	for _, profile := range profiles {
		var shouldSkip bool
		for _, pattern := range skipFile {
			matched, err := regexp.MatchString(pattern, profile.FileName)
			if err != nil {
				return nil, fmt.Errorf("filterProfile failed with pattern %s for profile %s, err: %v", pattern, profile.FileName, err)
			}

			if matched {
				shouldSkip = true
				break // no need to check again for the file
			}
		}

		if !shouldSkip {
			out = append(out, profile)
		}
	}

	return out, nil
}

func (s *server) clear(c *gin.Context) {
	var body ProfileParam
	if err := c.ShouldBind(&body); err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}
	svrsUnderTest := s.Store.GetAll()
	filterAddrInfoList, err := filterAddrInfo(body.Service, body.Address, true, svrsUnderTest)
	if err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}
	for _, addrInfo := range filterAddrInfoList {
		pp, err := NewWorker(addrInfo.Address).Clear(ProfileParam{})
		if err != nil {
			c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
			return
		}
		fmt.Fprintf(c.Writer, "Register service %s coverage counter %s", addrInfo.Address, string(pp))
	}

}

func (s *server) initSystem(c *gin.Context) {
	if err := s.Store.Init(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, "")
}

func (s *server) removeServices(c *gin.Context) {
	var body ProfileParam
	if err := c.ShouldBind(&body); err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}
	svrsUnderTest := s.Store.GetAll()
	filterAddrInfoList, err := filterAddrInfo(body.Service, body.Address, true, svrsUnderTest)
	if err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}
	for _, addrInfo := range filterAddrInfoList {
		err := s.Store.Remove(addrInfo.Address)
		if err != nil {
			c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
			return
		}
		fmt.Fprintf(c.Writer, "Register service %s removed from the center.", addrInfo.Address)
	}
}

func convertProfile(p []byte) ([]*cover.Profile, error) {
	// Annoyingly, ParseProfiles only accepts a filename, so we have to write the bytes to disk
	// so it can read them back.
	// We could probably also just give it /dev/stdin, but that'll break on Windows.
	tf, err := os.CreateTemp("", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file, err: %v", err)
	}
	defer tf.Close()
	defer os.Remove(tf.Name())
	if _, err := io.Copy(tf, bytes.NewReader(p)); err != nil {
		return nil, fmt.Errorf("failed to copy data to temp file, err: %v", err)
	}

	return cover.ParseProfiles(tf.Name())
}

func contains(arr []string, str string) bool {
	for _, element := range arr {
		if str == element {
			return true
		}
	}
	return false
}

// filterAddrInfo filter address list by given service and address list
func filterAddrInfo(serviceList, addressList []string, force bool, allInfos map[string][]string) (filterAddrList []ServiceUnderTest, err error) {
	addressAll := []string{}
	for _, addr := range allInfos {
		addressAll = append(addressAll, addr...)
	}

	if len(serviceList) != 0 && len(addressList) != 0 {
		return nil, fmt.Errorf("use 'service' flag and 'address' flag at the same time may cause ambiguity, please use them separately")
	}

	// Add matched services to map
	for _, name := range serviceList {
		if addrs, ok := allInfos[name]; ok {
			for _, addr := range addrs {
				filterAddrList = append(filterAddrList, ServiceUnderTest{Name: name, Address: addr})
			}
			continue // jump to match the next service
		}
		if !force {
			return nil, fmt.Errorf("service [%s] not found", name)
		}
		log.Warnf("service [%s] not found", name)
	}

	// Add matched addresses to map
	for _, addr := range addressList {
		if contains(addressAll, addr) {
			filterAddrList = append(filterAddrList, ServiceUnderTest{Address: addr})
			continue
		}
		if !force {
			return nil, fmt.Errorf("address [%s] not found", addr)
		}
		log.Warnf("address [%s] not found", addr)
	}

	if len(addressList) == 0 && len(serviceList) == 0 {
		for _, addr := range addressAll {
			filterAddrList = append(filterAddrList, ServiceUnderTest{Address: addr})
		}
	}

	// Return all services when all param is nil
	return filterAddrList, nil
}
