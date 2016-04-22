package connector

import (
	"crypto/tls"
	"crypto/x509"

	"gopkg.in/ldap.v2"

	"fmt"

	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/coreos/dex/pkg/log"
	"github.com/coreos/go-oidc/jose"
	"github.com/coreos/go-oidc/oidc"
)

//LDAP type constants
const (
	LDAPConnectorType         = "ldap"
	LDAPLoginPageTemplateName = "ldap-login.html"
)

func init() {
	RegisterConnectorConfigType(LDAPConnectorType, func() ConnectorConfig { return &LDAPConnectorConfig{} })
}

//LDAPConnectorConfig is the configuration for an ldap connector
type LDAPConnectorConfig struct {
	ID                   string        `json:"id"`
	ServerHost           string        `json:"serverHost"`
	ServerPort           uint16        `json:"serverPort"`
	Timeout              time.Duration `json:"timeout"`
	UseTLS               bool          `json:"useTLS"`
	UseSSL               bool          `json:"useSSL"`
	CertFile             string        `json:"certFile"`
	KeyFile              string        `json:"keyFile"`
	CaFile               string        `json:"caFile"`
	SkipCertVerification bool          `json:"skipCertVerification"`
	BaseDN               string        `json:"baseDN"`
	NameAttribute        string        `json:"nameAttribute"`
	EmailAttribute       string        `json:"emailAttribute"`
	SearchBeforeAuth     bool          `json:"searchBeforeAuth"`
	SearchFilter         string        `json:"searchFilter"`
	SearchScope          string        `json:"searchScope"`
	SearchBindDN         string        `json:"searchBindDN"`
	SearchBindPw         string        `json:"searchBindPw"`
	BindTemplate         string        `json:"bindTemplate"`
	TrustedEmailProvider bool          `json:"trustedEmailProvider"`

	//Attributes is a map of string to string, where the keys are the LDAP source
	//of additional claims and the values are the JWT destination claim names
	Attributes map[string]string `json:"attributes"`
	ldapAttrs  []string
}

//ConnectorID implements ConnectorConfig.ConnectorID
func (cfg *LDAPConnectorConfig) ConnectorID() string {
	return cfg.ID
}

//ConnectorType implements ConnectorConfig.ConnectorType
func (cfg *LDAPConnectorConfig) ConnectorType() string {
	return LDAPConnectorType
}

//LDAPConnector is the abstraction for connecting to an LDAP authentication and
//attribute/claim storage backend
type LDAPConnector struct {
	id                   string
	idp                  *LDAPIdentityProvider
	namespace            url.URL
	trustedEmailProvider bool
	loginFunc            oidc.LoginFunc
	loginTpl             *template.Template
}

func (cfg *LDAPConnectorConfig) Connector(ns url.URL, lf oidc.LoginFunc, tpls *template.Template) (Connector, error) {
	ns.Path = path.Join(ns.Path, httpPathCallback)
	tpl := tpls.Lookup(LDAPLoginPageTemplateName)
	if tpl == nil {
		return nil, fmt.Errorf("unable to find necessary HTML template")
	}

	// defaults
	const defaultNameAttribute = "cn"
	const defaultEmailAttribute = "mail"
	const defaultBindTemplate = "uid=%u,%b"
	const defaultSearchScope = ldap.ScopeWholeSubtree

	if cfg.UseTLS && cfg.UseSSL {
		return nil, fmt.Errorf("Invalid configuration. useTLS and useSSL are mutual exclusive.")
	}

	if len(cfg.CertFile) > 0 && len(cfg.KeyFile) == 0 {
		return nil, fmt.Errorf("Invalid configuration. Both certFile and keyFile must be specified.")
	}

	nameAttribute := defaultNameAttribute
	if len(cfg.NameAttribute) > 0 {
		nameAttribute = cfg.NameAttribute
	}

	emailAttribute := defaultEmailAttribute
	if len(cfg.EmailAttribute) > 0 {
		emailAttribute = cfg.EmailAttribute
	}

	bindTemplate := defaultBindTemplate
	if len(cfg.BindTemplate) > 0 {
		if cfg.SearchBeforeAuth {
			log.Warningf("bindTemplate not used when searchBeforeAuth specified.")
		}
		bindTemplate = cfg.BindTemplate
	}

	searchScope := defaultSearchScope
	if len(cfg.SearchScope) > 0 {
		switch {
		case strings.EqualFold(cfg.SearchScope, "BASE"):
			searchScope = ldap.ScopeBaseObject
		case strings.EqualFold(cfg.SearchScope, "ONE"):
			searchScope = ldap.ScopeSingleLevel
		case strings.EqualFold(cfg.SearchScope, "SUB"):
			searchScope = ldap.ScopeWholeSubtree
		default:
			return nil, fmt.Errorf("Invalid value for searchScope: '%v'. Must be one of 'base', 'one' or 'sub'.", cfg.SearchScope)
		}
	}

	if cfg.Timeout != 0 {
		ldap.DefaultTimeout = cfg.Timeout * time.Millisecond
	}

	tlsConfig := &tls.Config{
		ServerName:         cfg.ServerHost,
		InsecureSkipVerify: cfg.SkipCertVerification,
	}

	if (cfg.UseTLS || cfg.UseSSL) && len(cfg.CaFile) > 0 {
		buf, err := ioutil.ReadFile(cfg.CaFile)
		if err != nil {
			return nil, err
		}

		rootCertPool := x509.NewCertPool()
		ok := rootCertPool.AppendCertsFromPEM(buf)
		if ok {
			tlsConfig.RootCAs = rootCertPool
		} else {
			return nil, fmt.Errorf("%v: Unable to parse certificate data.", cfg.CaFile)
		}
	}

	if (cfg.UseTLS || cfg.UseSSL) && len(cfg.CertFile) > 0 && len(cfg.KeyFile) > 0 {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	cfg.ldapAttrs = make([]string, 0, len(cfg.Attributes))
	for key := range cfg.Attributes {
		cfg.ldapAttrs = append(cfg.ldapAttrs, key)
	}
	idp := &LDAPIdentityProvider{
		cfg:              *cfg,
		serverHost:       cfg.ServerHost,
		serverPort:       cfg.ServerPort,
		useTLS:           cfg.UseTLS,
		useSSL:           cfg.UseSSL,
		nameAttribute:    nameAttribute,
		emailAttribute:   emailAttribute,
		searchBeforeAuth: cfg.SearchBeforeAuth,
		searchFilter:     cfg.SearchFilter,
		searchScope:      searchScope,
		searchBindDN:     cfg.SearchBindDN,
		searchBindPw:     cfg.SearchBindPw,
		bindTemplate:     bindTemplate,
		tlsConfig:        tlsConfig,
	}

	idpc := &LDAPConnector{
		id:                   cfg.ID,
		idp:                  idp,
		namespace:            ns,
		trustedEmailProvider: cfg.TrustedEmailProvider,
		loginFunc:            lf,
		loginTpl:             tpl,
	}

	return idpc, nil
}

func (c *LDAPConnector) ID() string {
	return c.id
}

func (c *LDAPConnector) Healthy() error {
	ldapConn, err := c.idp.LDAPConnect()
	if err == nil {
		ldapConn.Close()
	}
	return err
}

func (c *LDAPConnector) LoginURL(sessionKey, prompt string) (string, error) {
	q := url.Values{}
	q.Set("session_key", sessionKey)
	q.Set("prompt", prompt)
	enc := q.Encode()

	return path.Join(c.namespace.Path, "login") + "?" + enc, nil
}

func (c *LDAPConnector) Register(mux *http.ServeMux, errorURL url.URL) {
	route := path.Join(c.namespace.Path, "login")
	mux.Handle(route, handleLoginFunc(c.loginFunc, c.loginTpl, c.idp, route, errorURL))
}

func (c *LDAPConnector) Sync() chan struct{} {
	return make(chan struct{})
}

func (c *LDAPConnector) TrustedEmailProvider() bool {
	return c.trustedEmailProvider
}

type LDAPIdentityProvider struct {
	cfg              LDAPConnectorConfig
	serverHost       string
	serverPort       uint16
	useTLS           bool
	useSSL           bool
	baseDN           string
	nameAttribute    string
	emailAttribute   string
	searchBeforeAuth bool
	searchFilter     string
	searchScope      int
	searchBindDN     string
	searchBindPw     string
	bindTemplate     string
	tlsConfig        *tls.Config
}

func (m *LDAPIdentityProvider) LDAPConnect() (*ldap.Conn, error) {
	var err error
	var ldapConn *ldap.Conn

	log.Debugf("LDAPConnect()")
	if m.useSSL {
		ldapConn, err = ldap.DialTLS("tcp", fmt.Sprintf("%s:%d", m.serverHost, m.serverPort), m.tlsConfig)
		if err != nil {
			return nil, err
		}
	} else {
		ldapConn, err = ldap.Dial("tcp", fmt.Sprintf("%s:%d", m.serverHost, m.serverPort))
		if err != nil {
			return nil, err
		}
		if m.useTLS {
			err = ldapConn.StartTLS(m.tlsConfig)
			if err != nil {
				return nil, err
			}
		}
	}

	return ldapConn, err
}

//ParseString parses config variables into the search string provided in the config.
func (m *LDAPIdentityProvider) ParseString(template, username string) string {
	return strings.NewReplacer("%u", username, "%b", m.cfg.BaseDN).Replace(template)
}

func (m *LDAPIdentityProvider) Identity(username, password string) (*oidc.Identity, error) {
	var err error
	var bindDN, ldapUid, ldapName, ldapEmail string
	var claims jose.Claims
	var ldapConn *ldap.Conn

	ldapConn, err = m.LDAPConnect()
	if err != nil {
		return nil, err
	}
	defer ldapConn.Close()

	if m.searchBeforeAuth {
		err = ldapConn.Bind(m.searchBindDN, m.searchBindPw)
		if err != nil {
			return nil, err
		}

		filter := m.ParseString(m.searchFilter, username)

		attributes := make([]string, 0, len(m.cfg.ldapAttrs)+2)
		attributes = append(attributes, m.nameAttribute, m.emailAttribute)
		attributes = append(attributes, m.cfg.ldapAttrs...)

		s := ldap.NewSearchRequest(m.cfg.BaseDN, m.searchScope, ldap.NeverDerefAliases, 0, 0, false, filter, attributes, nil)

		sr, err := ldapConn.Search(s)
		if err != nil {
			return nil, err
		}
		if len(sr.Entries) == 0 {
			err = fmt.Errorf("Search returned no match. filter='%v' base='%v'", filter, m.cfg.BaseDN)
			return nil, err
		}

		bindDN = sr.Entries[0].DN
		ldapName = sr.Entries[0].GetAttributeValue(m.nameAttribute)
		ldapEmail = sr.Entries[0].GetAttributeValue(m.emailAttribute)

		for _, attr := range sr.Entries[0].Attributes {
			claims.Add(m.cfg.Attributes[attr.Name], attr.Values)
		}

		// drop to anonymous bind, prepare for bind as user
		err = ldapConn.Bind("", "")
		if err != nil {
			// unsupported or disallowed, reconnect
			log.Warningf("Re-connecting to LDAP Server after failure to bind anonymously: %v", err)
			ldapConn.Close()
			ldapConn, err = m.LDAPConnect()
			if err != nil {
				return nil, err
			}
		}
	} else {
		bindDN = m.ParseString(m.bindTemplate, username)
	}

	// authenticate user
	err = ldapConn.Bind(bindDN, password)
	if err != nil {
		return nil, err
	}

	ldapUid = bindDN

	return &oidc.Identity{
		ID:     ldapUid,
		Name:   ldapName,
		Email:  ldapEmail,
		Claims: claims,
	}, nil
}
