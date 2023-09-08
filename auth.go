package pulsar

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http"
)

type authProvider struct {
	AccessID  string
	AccessKey string
	rt        http.RoundTripper
}

func NewAuthProvider(accessID, accessKey string) *authProvider {
	return &authProvider{
		AccessID:  accessID,
		AccessKey: accessKey,
	}
}

func (a *authProvider) GetData() ([]byte, error) {
	key := md5Hex(a.AccessID + md5Hex(a.AccessKey))
	key = key[8:24]
	return []byte(fmt.Sprintf(`{"username":"%s","password":"%s"}`, a.AccessID, key)), nil
}

func md5Hex(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func (a *authProvider) Init() error {
	return nil
}

func (a *authProvider) Name() string {
	return "auth1"
}

func (a *authProvider) GetTLSCertificate() (*tls.Certificate, error) {
	return nil, nil
}

func (a *authProvider) Close() error {
	return nil
}

func (a *authProvider) RoundTrip(req *http.Request) (*http.Response, error) {
	return a.rt.RoundTrip(req)
}

func (a *authProvider) Transport() http.RoundTripper {
	return a.rt
}

func (a *authProvider) WithTransport(tr http.RoundTripper) error {
	a.rt = tr
	return nil
}
