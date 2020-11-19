// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package sharing

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/memory"
	"storj.io/common/ranger/httpranger"
	"storj.io/linksharing/objectmap"
	"storj.io/linksharing/objectranger"
	"storj.io/uplink"
	"storj.io/uplink/private/object"
)

var mon = monkit.Package()

// Config specifies the handler configuration.
type Config struct {
	// URLBase is the base URL of the link sharing handler. It is used
	// to construct URLs returned to clients. It should be a fully formed URL.
	URLBase string

	// Templates location with html templates.
	Templates string

	// TxtRecordTTL is the duration for which an entry in the txtRecordCache is valid.
	TxtRecordTTL time.Duration

	// AuthServiceConfig contains configuration required to use the auth service to resolve
	// access key ids into access grants.
	AuthServiceConfig AuthServiceConfig

	// DNS Server address, for TXT record lookup
	DNSServer string
}

// Location represents geographical points
// in the globe.
type Location struct {
	Latitude  float64
	Longitude float64
}

// Handler implements the link sharing HTTP handler.
//
// architecture: Service
type Handler struct {
	log        *zap.Logger
	urlBase    *url.URL
	templates  *template.Template
	mapper     *objectmap.IPDB
	txtRecords *txtRecords
	authConfig AuthServiceConfig
}

// NewHandler creates a new link sharing HTTP handler.
func NewHandler(log *zap.Logger, mapper *objectmap.IPDB, config Config) (*Handler, error) {
	dns, err := NewDNSClient(config.DNSServer)
	if err != nil {
		return nil, err
	}

	urlBase, err := parseURLBase(config.URLBase)
	if err != nil {
		return nil, err
	}

	if config.Templates == "" {
		config.Templates = "./web/*.html"
	}
	templates, err := template.ParseGlob(config.Templates)
	if err != nil {
		return nil, err
	}

	return &Handler{
		log:        log,
		urlBase:    urlBase,
		templates:  templates,
		mapper:     mapper,
		txtRecords: newTxtRecords(config.TxtRecordTTL, dns, config.AuthServiceConfig),
		authConfig: config.AuthServiceConfig,
	}, nil
}

// ServeHTTP handles link sharing requests.
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// serveHTTP handles the request in full. the error that is returned can
	// be ignored since it was only added to facilitate monitoring.
	_ = handler.serveHTTP(w, r)
}

func (handler *Handler) serveHTTP(w http.ResponseWriter, r *http.Request) (err error) {
	ctx := r.Context()
	defer mon.Task()(&ctx)(&err)

	equal, err := compareHosts(r.Host, handler.urlBase.Host)
	if err != nil {
		return err
	}

	if !equal {
		return handler.handleHostingService(ctx, w, r)
	}

	locationOnly := false

	switch r.Method {
	case http.MethodHead:
		locationOnly = true
	case http.MethodGet:
	default:
		err = errors.New("method not allowed")
		http.Error(w, err.Error(), http.StatusMethodNotAllowed)
		return err
	}

	return handler.handleTraditional(ctx, w, r, locationOnly)
}

func compareHosts(url1, url2 string) (equal bool, err error) {
	host1, _, err1 := net.SplitHostPort(url1)
	host2, _, err2 := net.SplitHostPort(url2)

	if err1 != nil && strings.Contains(err1.Error(), "missing port in address") {
		host1 = url1
	} else if err1 != nil {
		return false, err1
	}

	if err2 != nil && strings.Contains(err2.Error(), "missing port in address") {
		host2 = url2
	} else if err2 != nil {
		return false, err2
	}

	if host1 != host2 {
		return false, nil
	}
	return true, nil
}

// handleTraditional deals with normal linksharing that is accessed with the URL generated by the uplink share command.
func (handler *Handler) handleTraditional(ctx context.Context, w http.ResponseWriter, r *http.Request, locationOnly bool) error {
	rawRequest, access, serializedAccess, bucket, key, err := parseRequestPath(r.URL.Path, handler.authConfig)
	if err != nil {
		err = fmt.Errorf("invalid request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return err
	}

	p, err := uplink.OpenProject(ctx, access)
	if err != nil {
		handler.handleUplinkErr(w, "open project", err)
		return err
	}
	defer func() {
		if err := p.Close(); err != nil {
			handler.log.With(zap.Error(err)).Warn("unable to close project")
		}
	}()

	if key == "" || strings.HasSuffix(key, "/") {
		if !strings.HasSuffix(r.URL.Path, "/") {
			// Call redirect because directories must have a trailing '/' for the listed hyperlinks to generate correctly.
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return nil
		}
		err = handler.servePrefix(ctx, w, p, breadcrumb{
			Prefix: bucket,
			URL:    "/" + serializedAccess + "/" + bucket + "/",
		}, bucket, bucket, key, key)
		if err != nil {
			handler.handleUplinkErr(w, "list prefix", err)
		}
		return nil
	}

	o, err := p.StatObject(ctx, bucket, key)
	if err != nil {
		handler.handleUplinkErr(w, "stat object", err)
		return err
	}

	if locationOnly {
		location := makeLocation(handler.urlBase, r.URL.Path)
		http.Redirect(w, r, location, http.StatusFound)
		return nil
	}

	_, download := r.URL.Query()["download"]
	_, view := r.URL.Query()["view"]
	if !download && !view && !rawRequest {
		ipBytes, err := object.GetObjectIPs(ctx, uplink.Config{}, access, bucket, key)
		if err != nil {
			handler.handleUplinkErr(w, "get object IPs", err)
			return err
		}

		var locations []Location
		if handler.mapper != nil {
			for _, ip := range ipBytes {
				info, err := handler.mapper.GetIPInfos(string(ip))
				if err != nil {
					handler.log.Error("failed to get IP info", zap.Error(err))
					continue
				}

				location := Location{
					Latitude:  info.Location.Latitude,
					Longitude: info.Location.Longitude,
				}

				locations = append(locations, location)
			}
		}

		var input struct {
			Key       string
			Size      string
			Locations []Location
			Pieces    int64
		}
		input.Key = o.Key
		input.Size = memory.Size(o.System.ContentLength).Base10String()
		input.Locations = locations
		input.Pieces = int64(len(locations))

		return handler.templates.ExecuteTemplate(w, "single-object.html", input)
	}

	if download {
		segments := strings.Split(key, "/")
		obj := segments[len(segments)-1]
		w.Header().Set("Content-Disposition", "attachment; filename=\""+obj+"\"")
	}
	httpranger.ServeContent(ctx, w, r, key, o.System.Created, objectranger.New(p, o, bucket))
	return nil
}

type breadcrumb struct {
	Prefix string
	URL    string
}

func (handler *Handler) servePrefix(ctx context.Context, w http.ResponseWriter, project *uplink.Project, root breadcrumb, title, bucket, realPrefix, visiblePrefix string) (err error) {
	type Object struct {
		Key    string
		Size   string
		Prefix bool
	}

	var input struct {
		Title       string
		Breadcrumbs []breadcrumb
		Objects     []Object
	}
	input.Title = title
	input.Breadcrumbs = append(input.Breadcrumbs, root)
	if visiblePrefix != "" {
		trimmed := strings.TrimRight(visiblePrefix, "/")
		for i, prefix := range strings.Split(trimmed, "/") {
			input.Breadcrumbs = append(input.Breadcrumbs, breadcrumb{
				Prefix: prefix,
				URL:    input.Breadcrumbs[i].URL + prefix + "/",
			})
		}
	}

	input.Objects = make([]Object, 0)

	objects := project.ListObjects(ctx, bucket, &uplink.ListObjectsOptions{
		Prefix: realPrefix,
		System: true,
	})

	// TODO add paging
	for objects.Next() {
		item := objects.Item()
		key := item.Key[len(realPrefix):]
		input.Objects = append(input.Objects, Object{
			Key:    key,
			Size:   memory.Size(item.System.ContentLength).Base10String(),
			Prefix: item.IsPrefix,
		})
	}
	if objects.Err() != nil {
		return objects.Err()
	}

	return handler.templates.ExecuteTemplate(w, "prefix-listing.html", input)
}

func (handler *Handler) handleUplinkErr(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, uplink.ErrBucketNotFound):
		w.WriteHeader(http.StatusNotFound)
		err = handler.templates.ExecuteTemplate(w, "404.html", "Oops! Bucket not found.")
		if err != nil {
			handler.log.Error("error while executing template", zap.Error(err))
		}
	case errors.Is(err, uplink.ErrObjectNotFound):
		w.WriteHeader(http.StatusNotFound)
		err = handler.templates.ExecuteTemplate(w, "404.html", "Oops! Object not found.")
		if err != nil {
			handler.log.Error("error while executing template", zap.Error(err))
		}
	default:
		handler.log.Error("unable to handle request", zap.String("action", action), zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
	}
}

const versionAccessKeyID = 1 // we don't want to import stargate just for this

func parseAccess(access string, cfg AuthServiceConfig) (*uplink.Access, error) {
	// check if the serializedAccess is actually an access key id
	if _, version, err := base58.CheckDecode(access); err != nil {
		return nil, errs.New("invalid access")
	} else if version == versionAccessKeyID {
		authResp, err := cfg.Resolve(access)
		if err != nil {
			return nil, err
		}
		if !authResp.Public {
			return nil, errs.New("non-public access key id")
		}
		access = authResp.AccessGrant
	} else if version == 0 { // 0 could be any number of things, but we just assume an access
	} else {
		return nil, errs.New("invalid access version")
	}

	return uplink.ParseAccess(access)
}

func parseRequestPath(p string, cfg AuthServiceConfig) (rawRequest bool, _ *uplink.Access, serializedAccess, bucket, key string, err error) {
	// Drop the leading slash, if necessary.
	p = strings.TrimPrefix(p, "/")

	// Split the request path.
	segments := strings.SplitN(p, "/", 4)
	if len(segments) == 4 {
		if segments[0] == "raw" {
			rawRequest = true
			segments = segments[1:]
		} else {
			// If its not a raw request, we need to concat the last two entries as those contain paths in the bucket
			// and shrink the array again.
			rawRequest = false
			segments[2] = segments[2] + "/" + segments[3]
			segments = segments[:len(segments)-1]
		}
	}
	if len(segments) == 1 {
		if segments[0] == "" {
			return rawRequest, nil, "", "", "", errs.New("missing access")
		}
		return rawRequest, nil, "", "", "", errs.New("missing bucket")
	}

	serializedAccess = segments[0]

	bucket = segments[1]

	if len(segments) == 3 {
		key = segments[2]
	}

	access, err := parseAccess(serializedAccess, cfg)
	if err != nil {
		return rawRequest, nil, "", "", "", err
	}

	return rawRequest, access, serializedAccess, bucket, key, nil
}

func parseURLBase(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("URL base must be http:// or https://")
	case u.Host == "":
		return nil, errors.New("URL base must contain host")
	case u.User != nil:
		return nil, errors.New("URL base must not contain user info")
	case u.RawQuery != "":
		return nil, errors.New("URL base must not contain query values")
	case u.Fragment != "":
		return nil, errors.New("URL base must not contain a fragment")
	}
	return u, nil
}

func makeLocation(base *url.URL, reqPath string) string {
	location := *base
	location.Path = path.Join(location.Path, reqPath)
	return location.String()
}

// handleHostingService deals with linksharing via custom URLs.
func (handler *Handler) handleHostingService(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil && strings.Contains(err.Error(), "missing port in address") {
		host = r.Host
	} else if err != nil {
		handler.log.Error("unable to handle request", zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
		return err
	}

	access, root, err := handler.txtRecords.fetchAccessForHost(ctx, host)
	if err != nil {
		handler.log.Error("unable to handle request", zap.Error(err))
		http.Error(w, "unable to handle request", http.StatusInternalServerError)
		return err
	}

	project, err := uplink.OpenProject(ctx, access)
	if err != nil {
		handler.handleUplinkErr(w, "open project", err)
		return err
	}
	defer func() {
		if err := project.Close(); err != nil {
			handler.log.With(zap.Error(err)).Warn("unable to close project")
		}
	}()

	bucket, key := determineBucketAndObjectKey(root, r.URL.Path)
	if key != "" { // there are no objects with the empty key
		o, err := project.StatObject(ctx, bucket, key)
		if err == nil {
			// the requested key exists
			httpranger.ServeContent(ctx, w, r, key, o.System.Created, objectranger.New(project, o, bucket))
			return nil
		}
		if !strings.HasSuffix(key, "/") || !errors.Is(err, uplink.ErrObjectNotFound) {
			// the requested key does not end in a slash, or there was an unknown error
			handler.handleUplinkErr(w, "stat object", err)
			return err
		}
	}

	// due to the above logic, by this point, the key is either exactly "" or ends in a "/"

	k := key + "index.html"
	o, err := project.StatObject(ctx, bucket, k)
	if err == nil {
		httpranger.ServeContent(ctx, w, r, k, o.System.Created, objectranger.New(project, o, bucket))
		return nil
	}
	if !errors.Is(err, uplink.ErrObjectNotFound) {
		handler.handleUplinkErr(w, "stat object", err)
		return err
	}

	err = handler.servePrefix(ctx, w, project, breadcrumb{Prefix: host, URL: "/"}, host, bucket, key, strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		handler.handleUplinkErr(w, "list prefix", err)
		return err
	}
	return nil
}

// determineBucketAndObjectKey is a helper function to parse storj_root and the url into the bucket and object key.
// For example, we have http://mydomain.com/prefix2/index.html with storj_root:bucket1/prefix1/
// The root path will be [bucket1, prefix1/]. Our bucket is named bucket1.
// Since the url has a path of /prefix2/index.html and the second half of the root path is prefix1,
// we get an object key of prefix1/prefix2/index.html. To make this work, the first (and only the
// first) prefix slash from the URL is stripped. Additionally, to aid security, if there is a non-empty
// prefix, it will have a suffix slash added to it if no trailing slash exists. See
// TestDetermineBucketAndObjectKey for many examples.
func determineBucketAndObjectKey(root, urlPath string) (bucket, key string) {
	parts := strings.SplitN(root, "/", 2)
	bucket = parts[0]
	prefix := ""
	if len(parts) > 1 {
		prefix = parts[1]
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return bucket, prefix + strings.TrimPrefix(urlPath, "/")
}
