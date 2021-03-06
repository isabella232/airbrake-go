package airbrake

import (
	"bytes"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"text/template"
)

var (
	ApiKey      = ""
	Endpoint    = "https://api.airbrake.io/notifier_api/v2/notices"
	Environment = "development"
	Verbose     = false

	// PrettyParams allows including request query/form parameters on the Environment tab
	// which is more readable than the raw text of the Parameters tab (in Errbit).
	// The param keys will be rendered as "?<param>" so they will sort together at the top of the tab.
	PrettyParams = false

	// RootPackage enables rendering of the backtrace with hyperlinks to the repository.
	// If set to the name of the root package of the project, e.g. github.com/user/project,
	// any file paths in the backtrace that contain that string will be converted
	// to the `[PROJECT_ROOT]/...` form, which triggers the hyperlinking in errbit.
	// This feature also requires the APP to have its Repository configured in errbit.
	RootPackage = ""

	// AppVersion determines which commit will be used for backtrace hyperlinks.
	// If unset, errbit defaults to `master`. For github it should be a branch name
	// or a commit hash.
	// One way to record the corresponding commit hash in a compiled binary
	// is to use the -X linker flag. (see https://golang.org/cmd/ld)
	AppVersion = ""

	sensitive     = regexp.MustCompile(`(?i)password|token|secret|key`)
	badResponse   = errors.New("Bad response")
	apiKeyMissing = errors.New("Please set the airbrake.ApiKey before doing calls")
	tmpl          = template.Must(template.New("error").Parse(source))
)

type Line struct {
	Function string
	File     string
	Line     int
}

// stack implements Stack, skipping N frames
func stacktrace(skip int) (lines []Line) {
	for i := skip; ; i++ {
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}

		item := Line{function(pc), locate(file), line}

		// ignore panic method
		if item.Function != "panic" {
			lines = append(lines, item)
		}
	}
	return
}

// function returns, if possible, the name of the function containing the PC.
func function(pc uintptr) string {
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "???"
	} else {
		return shorten(fn.Name())
	}
}

func shorten(name string) string {
	// The name includes the path name to the package, which is unnecessary
	// since the file name is already included.  Plus, it has center dots.
	// That is, we see
	//  runtime/debug.*T·ptrmethod
	// and want
	//  debug.*T.ptrmethod
	if period := strings.LastIndex(name, "/"); period >= 0 {
		name = name[period+1:]
	}
	name = strings.Replace(name, "·", ".", -1)
	return name
}

func locate(f string) string {
	if RootPackage == "" {
		return f
	}
	parts := strings.Split(f, RootPackage)
	if len(parts) == 2 {
		return "[PROJECT_ROOT]" + parts[1]
	} else {
		return f
	}
}

func post(params map[string]interface{}) error {
	buffer := bytes.NewBufferString("")

	if err := tmpl.Execute(buffer, params); err != nil {
		log.Printf("Airbrake error: %s", err)
		return err
	}

	if Verbose {
		log.Printf("Airbrake payload for endpoint %s: %s", Endpoint, buffer)
	}

	response, err := http.Post(Endpoint, "text/xml", buffer)
	if err != nil {
		log.Printf("Airbrake error: %s", err)
		return err
	}

	if Verbose {
		body, _ := ioutil.ReadAll(response.Body)
		log.Printf("response: %s", body)
	}
	response.Body.Close()

	if Verbose {
		log.Printf("Airbrake post: %s status code: %d", params["Error"], response.StatusCode)
	}

	return nil
}

func Error(e error, request *http.Request) error {
	if ApiKey == "" {
		return apiKeyMissing
	}

	return post(params(e, request))
}

func Notify(e error) error {
	if ApiKey == "" {
		return apiKeyMissing
	}

	return post(params(e, nil))
}

func params(e error, request *http.Request) map[string]interface{} {
	params := map[string]interface{}{
		"Class":       reflect.TypeOf(e).String(),
		"Error":       e,
		"ApiKey":      ApiKey,
		"ErrorName":   e.Error(),
		"Environment": Environment,
	}

	if params["Class"] == "" {
		params["Class"] = "Panic"
	}

	pwd, err := os.Getwd()
	if err == nil {
		params["Pwd"] = pwd
	}

	hostname, err := os.Hostname()
	if err == nil {
		params["Hostname"] = hostname
	}

	params["Backtrace"] = stacktrace(3)

	if request == nil || request.ParseForm() != nil {
		return params
	}

	// Compile relevant request parameters into a map.
	req := make(map[string]interface{})
	params["Request"] = req
	req["Component"] = ""
	req["Action"] = ""
	// Nested http Muxes muck with the URL, prefer RequestURI.
	if request.RequestURI != "" {
		req["URL"] = request.RequestURI
	} else {
		req["URL"] = request.URL
	}

	// Compile header parameters.
	header := make(map[string]string)
	req["Header"] = header
	header["REQUEST_METHOD"] = request.Method
	header["REQUEST_PROTOCOL"] = request.Proto
	for k, v := range request.Header {
		if !omit(k, v) {
			// errbit processes some entries, e.g. user agent, and expects
			// the keys to be uppercased, underscored and prefixed with HTTP_
			k := strings.ToUpper(strings.Replace(k, "-", "_", -1))
			header["HTTP_"+k] = v[0]
		}
	}
	// This allows errbit to hyperlink to specific commit in the app repo.
	if AppVersion != "" {
		header["APP_VERSION"] = AppVersion
	}

	// Compile query/form parameters.
	form := make(map[string]string)
	req["Form"] = form
	for k, v := range request.Form {
		if !omit(k, v) {
			form[k] = v[0]
			if PrettyParams {
				header["?"+k] = v[0]
			}
		}
	}

	return params
}

// omit checks the key, values for emptiness or sensitivity.
func omit(key string, values []string) bool {
	return len(key) == 0 || len(values) == 0 || len(values[0]) == 0 || sensitive.FindString(key) != ""
}

func CapturePanic(r *http.Request) {
	if rec := recover(); rec != nil {

		if err, ok := rec.(error); ok {
			log.Printf("Recording err %s", err)
			Error(err, r)
		} else if err, ok := rec.(string); ok {
			log.Printf("Recording string %s", err)
			Error(errors.New(err), r)
		}

		panic(rec)
	}
}

const source = `<?xml version="1.0" encoding="UTF-8"?>
<notice version="2.0">
  <api-key>{{ .ApiKey }}</api-key>
  <notifier>
    <name>Airbrake Golang</name>
    <version>0.0.1</version>
    <url>http://airbrake.io</url>
  </notifier>
  <error>
    <class>{{ html .Class }}</class>
    <message>{{ html .ErrorName }}</message>
    <backtrace>{{ range .Backtrace }}
      <line method="{{ html .Function}}" file="{{ html .File}}" number="{{.Line}}"/>{{ end }}
    </backtrace>
  </error>{{ with .Request }}
  <request>
    <url>{{html .URL}}</url>
    <component>{{ .Component }}</component>
    <action>{{ .Action }}</action>
    <params>{{ range $key, $value := .Form }}
      <var key="{{ $key }}">{{ $value }}</var>{{ end }}</params>
    <cgi-data>{{ range $key, $value := .Header }}
      <var key="{{ $key }}">{{ $value }}</var>{{ end }}</cgi-data>
  </request>{{ end }}
  <server-environment>
    <project-root>{{ html .Pwd }}</project-root>
    <environment-name>{{ .Environment }}</environment-name>
    <hostname>{{ html .Hostname }}</hostname>
  </server-environment>
</notice>`
