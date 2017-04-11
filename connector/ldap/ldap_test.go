package ldap

import (
	"bytes"
	"context"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/kylelemons/godebug/pretty"

	"github.com/coreos/dex/connector"
)

const envVar = "DEX_LDAP_TESTS"

// subtest is a login test against a given schema.
type subtest struct {
	// Name of the sub-test.
	name string

	// Password credentials, and if the connector should request
	// groups as well.
	username string
	password string
	groups   bool

	// Expected result of the login.
	wantErr   bool
	wantBadPW bool
	want      connector.Identity
}

func TestQuery(t *testing.T) {
	schema := `
dn: dc=example,dc=org
objectClass: dcObject
objectClass: organization
o: Example Company
dc: example

dn: ou=People,dc=example,dc=org
objectClass: organizationalUnit
ou: People

dn: cn=jane,ou=People,dc=example,dc=org
objectClass: person
objectClass: iNetOrgPerson
sn: doe
cn: jane
mail: janedoe@example.com
userpassword: foo

dn: cn=john,ou=People,dc=example,dc=org
objectClass: person
objectClass: iNetOrgPerson
sn: doe
cn: john
mail: johndoe@example.com
userpassword: bar
`
	c := &Config{}
	c.UserSearch.BaseDN = "ou=People,dc=example,dc=org"
	c.UserSearch.NameAttr = "cn"
	c.UserSearch.EmailAttr = "mail"
	c.UserSearch.IDAttr = "DN"
	c.UserSearch.Username = "cn"

	tests := []subtest{
		{
			name:     "validpassword",
			username: "jane",
			password: "foo",
			want: connector.Identity{
				UserID:        "cn=jane,ou=People,dc=example,dc=org",
				Username:      "jane",
				Email:         "janedoe@example.com",
				EmailVerified: true,
			},
		},
		{
			name:     "validpassword2",
			username: "john",
			password: "bar",
			want: connector.Identity{
				UserID:        "cn=john,ou=People,dc=example,dc=org",
				Username:      "john",
				Email:         "johndoe@example.com",
				EmailVerified: true,
			},
		},
		{
			name:      "invalidpassword",
			username:  "jane",
			password:  "badpassword",
			wantBadPW: true,
		},
		{
			name:      "invaliduser",
			username:  "idontexist",
			password:  "foo",
			wantBadPW: true, // Want invalid password, not a query error.
		},
	}

	runTests(t, schema, c, tests)
}

func TestGroupQuery(t *testing.T) {
	schema := `
dn: dc=example,dc=org
objectClass: dcObject
objectClass: organization
o: Example Company
dc: example

dn: ou=People,dc=example,dc=org
objectClass: organizationalUnit
ou: People

dn: cn=jane,ou=People,dc=example,dc=org
objectClass: person
objectClass: iNetOrgPerson
sn: doe
cn: jane
mail: janedoe@example.com
userpassword: foo

dn: cn=john,ou=People,dc=example,dc=org
objectClass: person
objectClass: iNetOrgPerson
sn: doe
cn: john
mail: johndoe@example.com
userpassword: bar

# Group definitions.

dn: ou=Groups,dc=example,dc=org
objectClass: organizationalUnit
ou: Groups

dn: cn=admins,ou=Groups,dc=example,dc=org
objectClass: groupOfNames
cn: admins
member: cn=john,ou=People,dc=example,dc=org
member: cn=jane,ou=People,dc=example,dc=org

dn: cn=developers,ou=Groups,dc=example,dc=org
objectClass: groupOfNames
cn: developers
member: cn=jane,ou=People,dc=example,dc=org
`
	c := &Config{}
	c.UserSearch.BaseDN = "ou=People,dc=example,dc=org"
	c.UserSearch.NameAttr = "cn"
	c.UserSearch.EmailAttr = "mail"
	c.UserSearch.IDAttr = "DN"
	c.UserSearch.Username = "cn"
	c.GroupSearch.BaseDN = "ou=Groups,dc=example,dc=org"
	c.GroupSearch.UserAttr = "DN"
	c.GroupSearch.GroupAttr = "member"
	c.GroupSearch.NameAttr = "cn"

	tests := []subtest{
		{
			name:     "validpassword",
			username: "jane",
			password: "foo",
			groups:   true,
			want: connector.Identity{
				UserID:        "cn=jane,ou=People,dc=example,dc=org",
				Username:      "jane",
				Email:         "janedoe@example.com",
				EmailVerified: true,
				Groups:        []string{"admins", "developers"},
			},
		},
		{
			name:     "validpassword2",
			username: "john",
			password: "bar",
			groups:   true,
			want: connector.Identity{
				UserID:        "cn=john,ou=People,dc=example,dc=org",
				Username:      "john",
				Email:         "johndoe@example.com",
				EmailVerified: true,
				Groups:        []string{"admins"},
			},
		},
	}

	runTests(t, schema, c, tests)
}

// runTests runs a set of tests against an LDAP schema. It does this by
// setting up an OpenLDAP server and injecting the provided scheme.
//
// The tests require the slapd and ldapadd binaries available in the host
// machine's PATH.
//
// The DEX_LDAP_TESTS must be set to "1"
func runTests(t *testing.T, schema string, config *Config, tests []subtest) {
	if os.Getenv(envVar) != "1" {
		t.Skipf("%s not set. Skipping test (run 'export %s=1' to run tests)", envVar, envVar)
	}

	for _, cmd := range []string{"slapd", "ldapadd"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Errorf("%s not available", cmd)
		}
	}

	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	configBytes := new(bytes.Buffer)

	if err := slapdConfigTmpl.Execute(configBytes, tmplData{tempDir, includes(t)}); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tempDir, "ldap.conf")
	if err := ioutil.WriteFile(configPath, configBytes.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(tempDir, "schema.ldap")
	if err := ioutil.WriteFile(schemaPath, []byte(schema), 0644); err != nil {
		t.Fatal(err)
	}

	socketPath := url.QueryEscape(filepath.Join(tempDir, "ldap.unix"))

	slapdOut := new(bytes.Buffer)

	cmd := exec.Command(
		"slapd",
		"-d", "any",
		"-h", "ldap://localhost:10363/ ldaps://localhost:10636/ ldapi://"+socketPath,
		"-f", configPath,
	)
	cmd.Stdout = slapdOut
	cmd.Stderr = slapdOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var (
		// Wait group finishes once slapd has exited.
		//
		// Use a wait group because multiple goroutines can't listen on
		// cmd.Wait(). It triggers the race detector.
		wg = new(sync.WaitGroup)
		// Ensure only one condition can set the slapdFailed boolean.
		once        = new(sync.Once)
		slapdFailed bool
	)

	wg.Add(1)
	go func() { cmd.Wait(); wg.Done() }()

	defer func() {
		if slapdFailed {
			// If slapd exited before it was killed, print its logs.
			t.Logf("%s\n", slapdOut)
		}
	}()

	go func() {
		wg.Wait()
		once.Do(func() { slapdFailed = true })
	}()

	defer func() {
		once.Do(func() { slapdFailed = false })
		cmd.Process.Kill()
		wg.Wait()
	}()

	// Wait for slapd to come up.
	time.Sleep(100 * time.Millisecond)

	ldapadd := exec.Command(
		"ldapadd", "-x",
		"-D", "cn=admin,dc=example,dc=org",
		"-w", "admin",
		"-f", schemaPath,
		"-H", "ldap://localhost:10363/",
	)
	if out, err := ldapadd.CombinedOutput(); err != nil {
		t.Errorf("ldapadd: %s", out)
		return
	}

	// Shallow copy.
	c := *config

	// We need to configure host parameters but don't want to overwrite user or
	// group search configuration.
	c.Host = "localhost:10363"
	c.InsecureNoSSL = true
	c.BindDN = "cn=admin,dc=example,dc=org"
	c.BindPW = "admin"

	l := &logrus.Logger{Out: ioutil.Discard, Formatter: &logrus.TextFormatter{}}

	conn, err := c.openConnector(l)
	if err != nil {
		t.Errorf("open connector: %v", err)
	}

	for _, test := range tests {
		if test.name == "" {
			t.Fatal("go a subtest with no name")
		}

		// Run the subtest.
		t.Run(test.name, func(t *testing.T) {
			s := connector.Scopes{OfflineAccess: true, Groups: test.groups}
			ident, validPW, err := conn.Login(context.Background(), s, test.username, test.password)
			if err != nil {
				if !test.wantErr {
					t.Fatalf("query failed: %v", err)
				}
				return
			}
			if test.wantErr {
				t.Fatalf("wanted query to fail")
			}

			if !validPW {
				if !test.wantBadPW {
					t.Fatalf("invalid password: %v", err)
				}
				return
			}

			if test.wantBadPW {
				t.Fatalf("wanted invalid password")
			}
			got := ident
			got.ConnectorData = nil

			if diff := pretty.Compare(test.want, got); diff != "" {
				t.Error(diff)
				return
			}

			// Verify that refresh tokens work.
			ident, err = conn.Refresh(context.Background(), s, ident)
			if err != nil {
				t.Errorf("refresh failed: %v", err)
			}

			got = ident
			got.ConnectorData = nil

			if diff := pretty.Compare(test.want, got); diff != "" {
				t.Errorf("after refresh: %s", diff)
			}
		})
	}
}

// Standard OpenLDAP schema files to include.
//
// These are copied from the /etc/openldap/schema directory.
var includeFiles = []string{
	"core.schema",
	"cosine.schema",
	"inetorgperson.schema",
	"misc.schema",
	"nis.schema",
	"openldap.schema",
}

// tmplData is the struct used to execute the SLAPD config template.
type tmplData struct {
	// Directory for database to be writen to.
	TempDir string
	// List of schema files to include.
	Includes []string
}

// Config template copied from:
// http://www.zytrax.com/books/ldap/ch5/index.html#step1-slapd
var slapdConfigTmpl = template.Must(template.New("").Parse(`
{{ range $i, $include := .Includes }}
include {{ $include }}
{{ end }}

# MODULELOAD definitions
# not required (comment out) before version 2.3
moduleload back_bdb.la

database bdb
suffix "dc=example,dc=org"

# root or superuser
rootdn "cn=admin,dc=example,dc=org"
rootpw admin
# The database directory MUST exist prior to running slapd AND 
# change path as necessary
directory	{{ .TempDir }}

# Indices to maintain for this directory
# unique id so equality match only
index	uid	eq
# allows general searching on commonname, givenname and email
index	cn,gn,mail eq,sub
# allows multiple variants on surname searching
index sn eq,sub
# sub above includes subintial,subany,subfinal
# optimise department searches
index ou eq
# if searches will include objectClass uncomment following
# index objectClass eq
# shows use of default index parameter
index default eq,sub
# indices missing - uses default eq,sub
index telephonenumber

# other database parameters
# read more in slapd.conf reference section
cachesize 10000
checkpoint 128 15
`))

func includes(t *testing.T) (paths []string) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	for _, f := range includeFiles {
		p := filepath.Join(wd, "testdata", f)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("failed to find schema file: %s %v", p, err)
		}
		paths = append(paths, p)
	}
	return
}