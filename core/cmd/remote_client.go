package cmd

import (
	"bytes"
	"encoding/json"
	stderrs "errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manyminds/api2go/jsonapi"
	"github.com/mitchellh/go-homedir"
	"github.com/pelletier/go-toml"
	"github.com/pkg/errors"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli"
	clipkg "github.com/urfave/cli"
	"go.uber.org/multierr"

	"github.com/smartcontractkit/chainlink/core/bridges"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/sessions"
	"github.com/smartcontractkit/chainlink/core/static"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/smartcontractkit/chainlink/core/web"
	webpresenters "github.com/smartcontractkit/chainlink/core/web/presenters"
)

func initRemoteConfigSubCmds(client *Client) []cli.Command {
	return []cli.Command{
		{
			Name:   "show",
			Usage:  "Show the application configuration",
			Action: client.ConfigV2,
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "user-only",
					Usage: "If set, show only the user-provided TOML configuration, omitting application defaults",
				},
			},
		},
		{
			Name:   "loglevel",
			Usage:  "Set log level",
			Action: client.SetLogLevel,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "level",
					Usage: "set log level for node (debug||info||warn||error)",
				},
			},
		},
		{
			Name:   "logsql",
			Usage:  "Enable/disable SQL statement logging",
			Action: client.SetLogSQL,
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "enable",
					Usage: "enable SQL logging",
				},
				cli.BoolFlag{
					Name:  "disable",
					Usage: "disable SQL logging",
				},
			},
		},
		{
			Name:  "validate",
			Usage: "DEPRECATED. Use `chainlink node validate`",
			Before: func(ctx *clipkg.Context) error {
				return client.errorOut(fmt.Errorf("Deprecated, use `chainlink node validate`"))
			},
			Hidden: true,
		},
	}
}

var (
	errUnauthorized = errors.New(http.StatusText(http.StatusUnauthorized))
	errForbidden    = errors.New(http.StatusText(http.StatusForbidden))
	errBadRequest   = errors.New(http.StatusText(http.StatusBadRequest))
)

// CreateExternalInitiator adds an external initiator
func (cli *Client) CreateExternalInitiator(c *clipkg.Context) (err error) {
	if c.NArg() != 1 && c.NArg() != 2 {
		return cli.errorOut(errors.New("create expects 1 - 2 arguments: a name and a url (optional)"))
	}

	var request bridges.ExternalInitiatorRequest
	request.Name = c.Args().Get(0)

	// process optional URL
	if c.NArg() == 2 {
		var reqURL *url.URL
		reqURL, err = url.ParseRequestURI(c.Args().Get(1))
		if err != nil {
			return cli.errorOut(err)
		}
		request.URL = (*models.WebURL)(reqURL)
	}

	requestData, err := json.Marshal(request)
	if err != nil {
		return cli.errorOut(err)
	}

	buf := bytes.NewBuffer(requestData)
	resp, err := cli.HTTP.Post("/v2/external_initiators", buf)
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()

	var ei webpresenters.ExternalInitiatorAuthentication
	err = cli.renderAPIResponse(resp, &ei)
	return err
}

// DeleteExternalInitiator removes an external initiator
func (cli *Client) DeleteExternalInitiator(c *clipkg.Context) (err error) {
	if !c.Args().Present() {
		return cli.errorOut(errors.New("Must pass the name of the external initiator to delete"))
	}

	resp, err := cli.HTTP.Delete("/v2/external_initiators/" + c.Args().First())
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()
	_, err = cli.parseResponse(resp)
	return err
}

func (cli *Client) getPage(requestURI string, page int, model interface{}) (err error) {
	uri, err := url.Parse(requestURI)
	if err != nil {
		return err
	}
	q := uri.Query()
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	uri.RawQuery = q.Encode()

	resp, err := cli.HTTP.Get(uri.String())
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()

	err = cli.deserializeAPIResponse(resp, model, &jsonapi.Links{})
	if err != nil {
		return err
	}
	err = cli.errorOut(cli.Render(model))
	return err
}

// RemoteLogin creates a cookie session to run remote commands.
func (cli *Client) RemoteLogin(c *clipkg.Context) error {
	lggr := cli.Logger.Named("RemoteLogin")
	sessionRequest, err := cli.buildSessionRequest(c.String("file"))
	if err != nil {
		return cli.errorOut(err)
	}
	_, err = cli.CookieAuthenticator.Authenticate(sessionRequest)
	if err != nil {
		return cli.errorOut(err)
	}
	err = cli.checkRemoteBuildCompatibility(lggr, c.Bool("bypass-version-check"), static.Version, static.Sha)
	if err != nil {
		return cli.errorOut(err)
	}
	fmt.Println("Successfully Logged In.")
	return nil
}

// Logout removes local and remote session.
func (cli *Client) Logout(c *clipkg.Context) (err error) {
	resp, err := cli.HTTP.Delete("/sessions")
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()
	err = cli.CookieAuthenticator.Logout()
	if err != nil {
		return cli.errorOut(err)
	}
	return nil
}

// ChangePassword prompts the user for the old password and a new one, then
// posts it to Chainlink to change the password.
func (cli *Client) ChangePassword(c *clipkg.Context) (err error) {
	req, err := cli.ChangePasswordPrompter.Prompt()
	if err != nil {
		return cli.errorOut(err)
	}

	requestData, err := json.Marshal(req)
	if err != nil {
		return cli.errorOut(err)
	}

	buf := bytes.NewBuffer(requestData)
	resp, err := cli.HTTP.Patch("/v2/user/password", buf)
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("Password updated.")
	case http.StatusConflict:
		fmt.Println("Old password did not match.")
	default:
		return cli.printResponseBody(resp)
	}
	return nil
}

// Profile will collect pprof metrics and store them in a folder.
func (cli *Client) Profile(c *clipkg.Context) error {
	seconds := c.Uint("seconds")
	baseDir := c.String("output_dir")

	genDir := filepath.Join(baseDir, fmt.Sprintf("debuginfo-%s", time.Now().Format(time.RFC3339)))

	err := os.Mkdir(genDir, 0o755)
	if err != nil {
		return cli.errorOut(err)
	}
	var wgPprof sync.WaitGroup
	vitals := []string{
		"allocs",       // A sampling of all past memory allocations
		"block",        // Stack traces that led to blocking on synchronization primitives
		"cmdline",      // The command line invocation of the current program
		"goroutine",    // Stack traces of all current goroutines
		"heap",         // A sampling of memory allocations of live objects.
		"mutex",        // Stack traces of holders of contended mutexes
		"profile",      // CPU profile.
		"threadcreate", // Stack traces that led to the creation of new OS threads
		"trace",        // A trace of execution of the current program.
	}
	wgPprof.Add(len(vitals))
	cli.Logger.Infof("Collecting profiles: %v", vitals)
	cli.Logger.Infof("writing debug info to %s", genDir)

	errs := make(chan error, len(vitals))
	for _, vt := range vitals {
		go func(vt string) {
			defer wgPprof.Done()
			uri := fmt.Sprintf("/v2/debug/pprof/%s?seconds=%d", vt, seconds)
			resp, err := cli.HTTP.Get(uri)
			if err != nil {
				errs <- fmt.Errorf("error collecting %s: %w", vt, err)
				return
			}
			defer func() {
				if resp.Body != nil {
					resp.Body.Close()
				}
			}()
			if resp.StatusCode == http.StatusUnauthorized {
				errs <- fmt.Errorf("error collecting %s: %w", vt, errUnauthorized)
				return
			}
			if resp.StatusCode == http.StatusBadRequest {
				// best effort to interpret the underlying problem
				pprofVersion := resp.Header.Get("X-Go-Pprof")
				if pprofVersion == "1" {
					b, err := io.ReadAll(resp.Body)
					if err != nil {
						errs <- fmt.Errorf("error collecting %s: %w", vt, errBadRequest)
						return
					}
					respContent := string(b)
					// taken from pprof.Profile https://github.com/golang/go/blob/release-branch.go1.20/src/net/http/pprof/pprof.go#L133
					if strings.Contains(respContent, "profile duration exceeds server's WriteTimeout") {
						errs <- fmt.Errorf("%w: %s", ErrProfileTooLong, respContent)
					} else {
						errs <- fmt.Errorf("error collecting %s: %w: %s", vt, errBadRequest, respContent)
					}
				} else {
					errs <- fmt.Errorf("error collecting %s: %w", vt, errBadRequest)
				}
				return
			}
			// write to file
			f, err := os.Create(filepath.Join(genDir, vt))
			if err != nil {
				errs <- fmt.Errorf("error creating file for %s: %w", vt, err)
				return
			}
			wc := utils.NewDeferableWriteCloser(f)
			defer wc.Close()

			_, err = io.Copy(wc, resp.Body)
			if err != nil {
				errs <- fmt.Errorf("error writing to file for %s: %w", vt, err)
				return
			}
			err = wc.Close()
			if err != nil {
				errs <- fmt.Errorf("error closing file for %s: %w", vt, err)
				return
			}
		}(vt)
	}
	wgPprof.Wait()
	close(errs)
	// Atmost one err is emitted per vital.
	cli.Logger.Infof("collected %d/%d profiles", len(vitals)-len(errs), len(vitals))
	if len(errs) > 0 {
		var merr error
		for err := range errs {
			merr = stderrs.Join(merr, err)
		}
		return cli.errorOut(fmt.Errorf("profile collection failed:\n%v", merr))
	}
	return nil
}

func (cli *Client) buildSessionRequest(flag string) (sessions.SessionRequest, error) {
	if len(flag) > 0 {
		return cli.FileSessionRequestBuilder.Build(flag)
	}
	return cli.PromptingSessionRequestBuilder.Build("")
}

func getTOMLString(s string) (string, error) {
	var val interface{}
	err := toml.Unmarshal([]byte(s), &val)
	if err == nil {
		return s, nil
	}

	buf, err := fromFile(s)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("invalid TOML or file not found '%s'", s)
	} else if err != nil {
		return "", fmt.Errorf("error reading from file '%s': %v", s, err)
	}
	return buf.String(), nil
}

func (cli *Client) parseResponse(resp *http.Response) ([]byte, error) {
	b, err := parseResponse(resp)
	if errors.Is(err, errUnauthorized) {
		return nil, cli.errorOut(multierr.Append(err, fmt.Errorf("your credentials may be missing, invalid or you may need to login first using the CLI via 'chainlink admin login'")))
	}

	if errors.Is(err, errForbidden) {
		return nil, cli.errorOut(multierr.Append(err, fmt.Errorf("this action requires %s privileges. The current user %s has '%s' role and cannot perform this action, login with a user that has '%s' role via 'chainlink admin login'", resp.Header.Get("forbidden-required-role"), resp.Header.Get("forbidden-provided-email"), resp.Header.Get("forbidden-provided-role"), resp.Header.Get("forbidden-required-role"))))
	}
	if err != nil {
		return nil, cli.errorOut(err)
	}
	return b, err
}

func (cli *Client) printResponseBody(resp *http.Response) error {
	b, err := parseResponse(resp)
	if err != nil {
		return cli.errorOut(err)
	}

	fmt.Println(string(b))
	return nil
}

func (cli *Client) renderAPIResponse(resp *http.Response, dst interface{}, headers ...string) error {
	var links jsonapi.Links
	if err := cli.deserializeAPIResponse(resp, dst, &links); err != nil {
		return cli.errorOut(err)
	}

	return cli.errorOut(cli.Render(dst, headers...))
}

func (cli *Client) ConfigV2(c *clipkg.Context) error {
	userOnly := c.Bool("user-only")
	s, err := cli.configV2Str(userOnly)
	if err != nil {
		return err
	}
	fmt.Println(s)
	return nil
}

func (cli *Client) configV2Str(userOnly bool) (string, error) {
	resp, err := cli.HTTP.Get(fmt.Sprintf("/v2/config/v2?userOnly=%t", userOnly))
	if err != nil {
		return "", cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()
	respPayload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", cli.errorOut(err)
	}
	if resp.StatusCode != 200 {
		return "", cli.errorOut(errors.Errorf("got HTTP status %d: %s", resp.StatusCode, respPayload))
	}
	var configV2Resource web.ConfigV2Resource
	err = web.ParseJSONAPIResponse(respPayload, &configV2Resource)
	if err != nil {
		return "", cli.errorOut(err)
	}
	return configV2Resource.Config, nil
}

func normalizePassword(password string) string {
	return url.QueryEscape(strings.TrimSpace(password))
}

// SetLogLevel sets the log level on the node
func (cli *Client) SetLogLevel(c *clipkg.Context) (err error) {
	logLevel := c.String("level")
	request := web.LogPatchRequest{Level: logLevel}
	requestData, err := json.Marshal(request)
	if err != nil {
		return cli.errorOut(err)
	}

	buf := bytes.NewBuffer(requestData)
	resp, err := cli.HTTP.Patch("/v2/log", buf)
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()

	var svcLogConfig webpresenters.ServiceLogConfigResource
	err = cli.renderAPIResponse(resp, &svcLogConfig)
	return err
}

// SetLogSQL enables or disables the log sql statements
func (cli *Client) SetLogSQL(c *clipkg.Context) (err error) {
	// Enforces selection of --enable or --disable
	if !c.Bool("enable") && !c.Bool("disable") {
		return cli.errorOut(errors.New("Must set logSql --enabled || --disable"))
	}

	// Sets logSql to true || false based on the --enabled flag
	logSql := c.Bool("enable")

	request := web.LogPatchRequest{SqlEnabled: &logSql}
	requestData, err := json.Marshal(request)
	if err != nil {
		return cli.errorOut(err)
	}

	buf := bytes.NewBuffer(requestData)
	resp, err := cli.HTTP.Patch("/v2/log", buf)
	if err != nil {
		return cli.errorOut(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			err = multierr.Append(err, cerr)
		}
	}()

	var svcLogConfig webpresenters.ServiceLogConfigResource
	err = cli.renderAPIResponse(resp, &svcLogConfig)
	return err
}

func getBufferFromJSON(s string) (*bytes.Buffer, error) {
	if gjson.Valid(s) {
		return bytes.NewBufferString(s), nil
	}

	buf, err := fromFile(s)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("invalid JSON or file not found '%s'", s)
	} else if err != nil {
		return nil, fmt.Errorf("error reading from file '%s': %v", s, err)
	}
	return buf, nil
}

func fromFile(arg string) (*bytes.Buffer, error) {
	dir, err := homedir.Expand(arg)
	if err != nil {
		return nil, err
	}
	file, err := os.ReadFile(dir)
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(file), nil
}

// deserializeAPIResponse is distinct from deserializeResponse in that it supports JSONAPI responses with Links
func (cli *Client) deserializeAPIResponse(resp *http.Response, dst interface{}, links *jsonapi.Links) error {
	b, err := cli.parseResponse(resp)
	if err != nil {
		return errors.Wrap(err, "parseResponse error")
	}
	if err = web.ParsePaginatedResponse(b, dst, links); err != nil {
		return cli.errorOut(err)
	}
	return nil
}

// parseErrorResponseBody parses response body from web API and returns a single string containing all errors
func parseErrorResponseBody(responseBody []byte) (string, error) {
	if responseBody == nil {
		return "Empty error message", nil
	}

	var errors models.JSONAPIErrors
	err := json.Unmarshal(responseBody, &errors)
	if err != nil || len(errors.Errors) == 0 {
		return "", err
	}

	var errorDetails strings.Builder
	errorDetails.WriteString(errors.Errors[0].Detail)
	for _, errorDetail := range errors.Errors[1:] {
		fmt.Fprintf(&errorDetails, "\n%s", errorDetail.Detail)
	}
	return errorDetails.String(), nil
}

func parseResponse(resp *http.Response) ([]byte, error) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return b, multierr.Append(errors.New(resp.Status), err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return b, errUnauthorized
	} else if resp.StatusCode == http.StatusForbidden {
		return b, errForbidden
	} else if resp.StatusCode >= http.StatusBadRequest {
		errorMessage, err := parseErrorResponseBody(b)
		if err != nil {
			return b, err
		}
		return b, errors.New(errorMessage)
	}
	return b, err
}

func (cli *Client) checkRemoteBuildCompatibility(lggr logger.Logger, onlyWarn bool, cliVersion, cliSha string) error {
	resp, err := cli.HTTP.Get("/v2/build_info")
	if err != nil {
		lggr.Warnw("Got error querying for version. Remote node version is unknown and CLI may behave in unexpected ways.", "err", err)
		return nil
	}
	b, err := parseResponse(resp)
	if err != nil {
		lggr.Warnw("Got error parsing http response for remote version. Remote node version is unknown and CLI may behave in unexpected ways.", "resp", resp, "err", err)
		return nil
	}

	var remoteBuildInfo map[string]string
	if err := json.Unmarshal(b, &remoteBuildInfo); err != nil {
		lggr.Warnw("Got error json parsing bytes from remote version response. Remote node version is unknown and CLI may behave in unexpected ways.", "bytes", b, "err", err)
		return nil
	}
	remoteVersion, remoteSha := remoteBuildInfo["version"], remoteBuildInfo["commitSHA"]

	remoteSemverUnset := remoteVersion == static.Unset || remoteVersion == "" || remoteSha == static.Unset || remoteSha == ""
	cliRemoteSemverMismatch := remoteVersion != cliVersion || remoteSha != cliSha

	if remoteSemverUnset || cliRemoteSemverMismatch {
		// Show a warning but allow mismatch
		if onlyWarn {
			lggr.Warnf("CLI build (%s@%s) mismatches remote node build (%s@%s), it might behave in unexpected ways", remoteVersion, remoteSha, cliVersion, cliSha)
			return nil
		}
		// Don't allow usage of CLI by unsetting the session cookie to prevent further requests
		if err2 := cli.CookieAuthenticator.Logout(); err2 != nil {
			cli.Logger.Debugw("CookieAuthenticator failed to logout", "err", err2)
		}
		return ErrIncompatible{CLIVersion: cliVersion, CLISha: cliSha, RemoteVersion: remoteVersion, RemoteSha: remoteSha}
	}
	return nil
}

// ErrIncompatible is returned when the cli and remote versions are not compatible.
type ErrIncompatible struct {
	CLIVersion, CLISha       string
	RemoteVersion, RemoteSha string
}

func (e ErrIncompatible) Error() string {
	return fmt.Sprintf("error: CLI build (%s@%s) mismatches remote node build (%s@%s). You can set flag --bypass-version-check to bypass this", e.CLIVersion, e.CLISha, e.RemoteVersion, e.RemoteSha)
}
