package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const securityFixtureSecret = `{"AccessKey":"fixture-access","SecretKey":"fixture-secret","SessionToken":"fixture-session-token"}`
const securityFixtureBuckets = `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Buckets><Bucket><Name>secure-fixture</Name></Bucket></Buckets></ListAllMyBucketsResult>`

// All credentials, configuration, and endpoints in these tests are local fixtures.
func securityFixtureApp(t *testing.T, endpoint string, trustedServers ...*httptest.Server) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "empty-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "empty-credentials"))
	for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_CA_BUNDLE"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "false")
	t.Setenv("AWS_USE_FIPS_ENDPOINT", "false")
	t.Setenv("AWS_USE_DUALSTACK_ENDPOINT", "false")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_RETRY_MODE", "standard")

	a := NewApp()
	a.ctx = context.Background()
	p := Profile{ID: "security-fixture", Name: "Security fixture", Region: "us-east-1", Endpoint: endpoint, PathStyle: true}
	if len(trustedServers) > 0 {
		var ca []byte
		for _, server := range trustedServers {
			ca = append(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})...)
		}
		p.CAFile = filepath.Join(dir, "fixture-ca.pem")
		if err := os.WriteFile(p.CAFile, ca, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.profiles = []Profile{p}
	a.secretGet = func(string) (string, error) { return securityFixtureSecret, nil }
	return a
}

func securityFixtureClient(t *testing.T, a *App) *s3.Client {
	t.Helper()
	c, err := a.client(context.Background(), "security-fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Options().HTTPClient.(*http.Client).CloseIdleConnections)
	return c
}

func securityFixtureListBuckets(c *s3.Client) (*s3.ListBucketsOutput, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ListBuckets(ctx, &s3.ListBucketsInput{})
}

func assertSecurityFixtureSignature(t *testing.T, r *http.Request) {
	t.Helper()
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-access/") {
		t.Error("fixture request is missing its AWS signature")
	}
	if r.Header.Get("X-Amz-Security-Token") != "fixture-session-token" {
		t.Error("fixture request is missing its session token")
	}
}

// Trustworthy localhost TLS makes the cross-host leak test meaningful even if
// redirect following is accidentally re-enabled.
func securityFixtureTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestS3HTTPSRejectsRedirects(t *testing.T) {
	for _, target := range []struct {
		name      string
		tls       bool
		crossHost bool
	}{
		{name: "http_same_host"},
		{name: "http_cross_host", crossHost: true},
		{name: "https_same_host", tls: true},
		{name: "https_cross_host", tls: true, crossHost: true},
	} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(target.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				var targetRequests, signedRequests atomic.Int32
				targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetRequests.Add(1)
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(securityFixtureBuckets))
				})
				var destination *httptest.Server
				if target.tls {
					destination = securityFixtureTLSServer(t, targetHandler)
				} else {
					destination = httptest.NewServer(targetHandler)
					t.Cleanup(destination.Close)
				}
				targetURL := destination.URL
				if target.crossHost {
					targetURL = strings.Replace(targetURL, "127.0.0.1", "localhost", 1)
				}
				source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					signedRequests.Add(1)
					assertSecurityFixtureSignature(t, r)
					http.Redirect(w, r, targetURL, status)
				}))
				t.Cleanup(source.Close)
				trusted := []*httptest.Server{source}
				if target.tls {
					trusted = append(trusted, destination)
				}
				c := securityFixtureClient(t, securityFixtureApp(t, source.URL, trusted...))
				_, err := securityFixtureListBuckets(c)
				if !errors.Is(err, errS3Redirect) {
					t.Fatalf("redirect must fail with endpoint guidance, got %v", err)
				}
				if signedRequests.Load() != 1 {
					t.Fatalf("expected one signed HTTPS request, got %d", signedRequests.Load())
				}
				if targetRequests.Load() != 0 {
					t.Fatalf("redirect destination received %d requests; credentials could leak", targetRequests.Load())
				}
			})
		}
	}
}

func TestS3HTTPSRejectsPlaintextEndpoints(t *testing.T) {
	for _, setting := range []string{"profile"} {
		t.Run(setting, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(securityFixtureBuckets))
			}))
			t.Cleanup(server.Close)
			endpoint := ""
			if setting == "profile" {
				endpoint = server.URL
			}
			a := securityFixtureApp(t, endpoint)
			if setting != "profile" {
				t.Setenv(setting, server.URL)
			}
			c := securityFixtureClient(t, a)
			if aws.ToString(c.Options().BaseEndpoint) != server.URL {
				t.Fatal("fixture endpoint was not selected; refusing to contact a non-fixture service")
			}
			_, err := securityFixtureListBuckets(c)
			if !errors.Is(err, errS3HTTPSRequired) {
				t.Fatalf("plaintext endpoint must fail before transmission, got %v", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("plaintext endpoint received %d requests", requests.Load())
			}
		})
	}
}

func TestS3HTTPSAllowsSignedRequestsWithCustomCA(t *testing.T) {
	for _, setting := range []string{"profile"} {
		t.Run(setting, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assertSecurityFixtureSignature(t, r)
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(securityFixtureBuckets))
			}))
			t.Cleanup(server.Close)
			endpoint := ""
			if setting == "profile" {
				endpoint = server.URL
			}
			a := securityFixtureApp(t, endpoint, server)
			if setting != "profile" {
				t.Setenv(setting, server.URL)
			}
			c := securityFixtureClient(t, a)
			if aws.ToString(c.Options().BaseEndpoint) != server.URL {
				t.Fatal("fixture endpoint was not selected; refusing to contact a non-fixture service")
			}
			out, err := securityFixtureListBuckets(c)
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 || len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "secure-fixture" {
				t.Fatalf("unexpected HTTPS bucket result: requests=%d buckets=%v", requests.Load(), out.Buckets)
			}
		})
	}
}

// Returning a synthetic HTTP response observes final endpoint resolution and
// signing without making any network connection, including to public AWS.
type endpointRecordingClient struct{ request *http.Request }

func (c *endpointRecordingClient) Do(r *http.Request) (*http.Response, error) {
	c.request = r
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/xml"}}, Body: io.NopCloser(strings.NewReader(securityFixtureBuckets)), Request: r}, nil
}

func TestS3IgnoresAmbientCustomEndpoints(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		for _, setting := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "shared-global", "shared-s3"} {
			for _, explicit := range []bool{false, true} {
				name := scheme + "/" + setting
				if explicit {
					name += "/explicit-profile"
				} else {
					name += "/aws-default"
				}
				t.Run(name, func(t *testing.T) {
					a := securityFixtureApp(t, "")
					ambient := scheme + "://127.0.0.1:1/ambient"
					switch setting {
					case "shared-global", "shared-s3":
						contents := "[default]\nendpoint_url = " + ambient + "\n"
						if setting == "shared-s3" {
							contents = "[default]\nservices = fixture\n[services fixture]\ns3 =\n  endpoint_url = " + ambient + "\n"
						}
						if err := os.WriteFile(os.Getenv("AWS_CONFIG_FILE"), []byte(contents), 0600); err != nil {
							t.Fatal(err)
						}
					default:
						t.Setenv(setting, ambient)
					}
					if explicit {
						a.profiles[0].Endpoint = "https://127.0.0.1:2/explicit"
					}
					c := securityFixtureClient(t, a)
					if got := aws.ToString(c.Options().BaseEndpoint); got != a.profiles[0].Endpoint {
						t.Fatalf("ambient endpoint selected: %q", got)
					}
					recorder := &endpointRecordingClient{}
					_, err := c.ListBuckets(context.Background(), &s3.ListBucketsInput{}, func(o *s3.Options) { o.HTTPClient = recorder })
					if err != nil {
						t.Fatal(err)
					}
					if recorder.request == nil {
						t.Fatal("request not observed")
					}
					wantHost := "s3.us-east-1.amazonaws.com"
					if explicit {
						wantHost = "127.0.0.1:2"
					}
					if recorder.request.URL.Scheme != "https" || recorder.request.URL.Host != wantHost {
						t.Fatalf("unexpected signed destination: %s", recorder.request.URL)
					}
					assertSecurityFixtureSignature(t, recorder.request)
				})
			}
		}
	}
}
