package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/go-ldap/ldap/v3"

	"sitemonitor/internal/config"
)

// LDAP authenticates against Active Directory (or any LDAP directory):
//
//  1. bind with the service account (bind_dn / bind_password)
//  2. search base_dn with user_filter to find the user's DN
//  3. bind as that DN with the user's password (this is the password check)
//  4. map group membership to a role via role_groups (optionally nested groups)
//
// Only TLS connections are allowed (ldaps:// or ldap:// + StartTLS).
type LDAP struct {
	cfg config.LDAPAuth
	tls *tls.Config
}

// AD "LDAP_MATCHING_RULE_IN_CHAIN": matches transitive (nested) group membership.
const matchingRuleInChain = "1.2.840.113556.1.4.1941"

func NewLDAP(cfg config.LDAPAuth) (*LDAP, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("auth.ldap.url: %w", err)
	}
	tcfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth.ldap.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auth.ldap.ca_file: no certificates found")
		}
		tcfg.RootCAs = pool
	}
	return &LDAP{cfg: cfg, tls: tcfg}, nil
}

func (l *LDAP) Name() string { return "ldap" }

func (l *LDAP) dial(ctx context.Context) (*ldap.Conn, error) {
	d := &net.Dialer{Timeout: l.cfg.Timeout}
	conn, err := ldap.DialURL(l.cfg.URL, ldap.DialWithDialer(d), ldap.DialWithTLSConfig(l.tls))
	if err != nil {
		return nil, fmt.Errorf("ldap connect: %w", err)
	}
	conn.SetTimeout(l.cfg.Timeout)
	if l.cfg.StartTLS && strings.HasPrefix(l.cfg.URL, "ldap://") {
		if err := conn.StartTLS(l.tls); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ldap starttls: %w", err)
		}
	}
	go func() { <-ctx.Done(); conn.Close() }()
	return conn, nil
}

func (l *LDAP) serviceBind(conn *ldap.Conn) error {
	if l.cfg.BindDN == "" {
		return conn.UnauthenticatedBind("")
	}
	if err := conn.Bind(l.cfg.BindDN, l.cfg.BindPassword); err != nil {
		return fmt.Errorf("ldap service-account bind failed (check bind_dn/bind_password): %w", err)
	}
	return nil
}

func (l *LDAP) Authenticate(ctx context.Context, username, password string) (*Identity, error) {
	if password == "" {
		return nil, ErrInvalidCredentials // an empty password would be an "unauthenticated bind" that succeeds
	}
	ctx, cancel := context.WithTimeout(ctx, 3*l.cfg.Timeout)
	defer cancel()
	conn, err := l.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := l.serviceBind(conn); err != nil {
		return nil, err
	}

	// 1. find the user
	filter := strings.ReplaceAll(l.cfg.UserFilter, "{username}", ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(l.cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		2, int(l.cfg.Timeout.Seconds()), false, filter, []string{"dn", "memberOf", l.cfg.DisplayAttr}, nil))
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("ldap user search: %w", err)
	}
	if len(res.Entries) != 1 {
		return nil, ErrInvalidCredentials // not found, or ambiguous filter
	}
	entry := res.Entries[0]

	// 2. verify the password by binding as the user
	if err := conn.Bind(entry.DN, password); err != nil {
		var lerr *ldap.Error
		if errors.As(err, &lerr) && lerr.ResultCode == ldap.LDAPResultInvalidCredentials {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("ldap user bind: %w", err)
	}

	// 3. map groups to a role (highest role wins); search as the service account again
	if err := l.serviceBind(conn); err != nil {
		return nil, err
	}
	role, err := l.role(conn, entry)
	if err != nil {
		return nil, err
	}
	display := entry.GetAttributeValue(l.cfg.DisplayAttr)
	if display == "" {
		display = username
	}
	return &Identity{Username: username, DisplayName: display, Role: role, Provider: "ldap"}, nil
}

func (l *LDAP) role(conn *ldap.Conn, user *ldap.Entry) (string, error) {
	memberOf := map[string]bool{}
	for _, g := range user.GetAttributeValues("memberOf") {
		memberOf[strings.ToLower(g)] = true
	}
	for _, role := range []string{config.RoleAdmin, config.RoleOperator, config.RoleViewer} {
		for _, groupDN := range l.cfg.RoleGroups[role] {
			if memberOf[strings.ToLower(groupDN)] {
				return role, nil
			}
			if !l.cfg.NestedGroups {
				continue
			}
			// Is the user a (transitive) member of this group?
			f := fmt.Sprintf("(member:%s:=%s)", matchingRuleInChain, ldap.EscapeFilter(user.DN))
			res, err := conn.Search(ldap.NewSearchRequest(groupDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				1, int(l.cfg.Timeout.Seconds()), false, f, []string{"dn"}, nil))
			if err == nil && len(res.Entries) > 0 {
				return role, nil
			}
		}
	}
	if l.cfg.DefaultRole != "" {
		return l.cfg.DefaultRole, nil
	}
	return "", ErrNoRole
}
