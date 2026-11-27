package project

import (
	"os"
	"strings"
	"testing"
)

func postgresService() Service {
	return Service{Name: "db", Image: "docker.io/library/postgres@sha256:" + strings.Repeat("c", 64),
		Env:  map[string]string{"POSTGRES_USER": "test", "POSTGRES_PASSWORD": "test", "PGDATA": "/var/lib/postgresql/data/pgdata"},
		User: 999, Port: 5432, ReadySeconds: 60, WritablePaths: []string{"/var/lib/postgresql/data", "/var/run/postgresql"},
		CPU: "500m", Memory: "512Mi", EphemeralStorage: "512Mi"}
}

func withServices(services ...Service) Manifest {
	m := generic()
	m.Verification.Services = services
	return m
}

func TestServicesRoundTripAndChangeOnlyOptedInDigests(t *testing.T) {
	m := withServices(postgresService())
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(m.Canonical())
	if err != nil || parsed.Digest() != m.Digest() || len(parsed.Verification.Services) != 1 ||
		parsed.Verification.Services[0].GroupID() != 999 || parsed.Verification.ServiceReadySeconds() != 60 {
		t.Fatal("round trip", err)
	}
	if m.Digest() == generic().Digest() || !strings.Contains(string(m.Canonical()), `"services":[{"name":"db",`) {
		t.Fatal("services must be bound into the canonical form", string(m.Canonical()))
	}
	// Optional service fields are omitted when unset.
	for _, absent := range []string{`"args"`, `"group"`, `"writableRootFilesystem"`} {
		if strings.Contains(string(m.Canonical()), absent) {
			t.Fatal("unset optional service field encoded", absent)
		}
	}
	if strings.Contains(string(generic().Canonical()), "services") || generic().Verification.ServiceReadySeconds() != 0 {
		t.Fatal("absent services are encoded")
	}
	for _, bad := range []string{
		strings.Replace(string(m.Canonical()), `"name":"db"`, `"name":"db","extra":1`, 1),
		strings.Replace(string(m.Canonical()), `"port":5432`, `"port":5432,"port":5433`, 1),
		strings.Replace(string(m.Canonical()), `"port":5432`, `"port":"5432"`, 1),
		strings.Replace(string(m.Canonical()), `"services":[`, `"services":[null,`, 1),
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestServiceGroupAndOptionalFieldsAreValidated(t *testing.T) {
	s := postgresService()
	group := int64(1001)
	s.Group, s.Args, s.WritableRootFilesystem = &group, []string{"-c", "fsync=off"}, true
	m := withServices(s)
	if err := m.Validate(); err != nil || s.GroupID() != 1001 {
		t.Fatal("a group, arguments and a writable root are reviewable", err)
	}
	if parsed, err := ParseManifest(m.Canonical()); err != nil || parsed.Verification.Services[0].GroupID() != 1001 ||
		!parsed.Verification.Services[0].WritableRootFilesystem {
		t.Fatal("optional service fields must round trip", err)
	}
}

func TestInvalidServicesAreRefused(t *testing.T) {
	negative, root := int64(-1), int64(0)
	for name, change := range map[string]func(*Service){
		"no name":          func(s *Service) { s.Name = "" },
		"upper name":       func(s *Service) { s.Name = "DB" },
		"long name":        func(s *Service) { s.Name = strings.Repeat("a", 16) },
		"trailing hyphen":  func(s *Service) { s.Name = "db-" },
		"mutable image":    func(s *Service) { s.Image = "docker.io/library/postgres:17" },
		"no image":         func(s *Service) { s.Image = "" },
		"root user":        func(s *Service) { s.User = 0 },
		"huge user":        func(s *Service) { s.User = 1 << 31 },
		"negative group":   func(s *Service) { s.Group = &negative },
		"root group":       func(s *Service) { s.Group = &root },
		"privileged port":  func(s *Service) { s.Port = 80 },
		"no port":          func(s *Service) { s.Port = 0 },
		"port range":       func(s *Service) { s.Port = 65536 },
		"no ready":         func(s *Service) { s.ReadySeconds = 0 },
		"long ready":       func(s *Service) { s.ReadySeconds = MaxServiceReadySeconds + 1 },
		"cpu":              func(s *Service) { s.CPU = "100" },
		"memory":           func(s *Service) { s.Memory = "1Ti" },
		"no storage":       func(s *Service) { s.EphemeralStorage = "" },
		"empty arg":        func(s *Service) { s.Args = []string{""} },
		"hidden arg":       func(s *Service) { s.Args = []string{"-c\u202e"} },
		"expanded arg":     func(s *Service) { s.Args = []string{"$(POSTGRES_PASSWORD)"} },
		"escaped arg":      func(s *Service) { s.Args = []string{"$$x"} },
		"many args":        func(s *Service) { s.Args = make([]string, MaxServiceArgs+1) },
		"lower env":        func(s *Service) { s.Env = map[string]string{"pgdata": "/x"} },
		"loader env":       func(s *Service) { s.Env = map[string]string{"LD_PRELOAD": "/x.so"} },
		"empty env":        func(s *Service) { s.Env = map[string]string{"POSTGRES_DB": ""} },
		"expanded env":     func(s *Service) { s.Env = map[string]string{"POSTGRES_DB": "$(HOSTNAME)"} },
		"hidden env":       func(s *Service) { s.Env = map[string]string{"POSTGRES_DB": "db\u200b"} },
		"long env":         func(s *Service) { s.Env = map[string]string{"POSTGRES_DB": strings.Repeat("x", MaxEnvValueBytes+1)} },
		"relative path":    func(s *Service) { s.WritablePaths = []string{"data"} },
		"root path":        func(s *Service) { s.WritablePaths = []string{"/"} },
		"unclean path":     func(s *Service) { s.WritablePaths = []string{"/var/../etc"} },
		"proc path":        func(s *Service) { s.WritablePaths = []string{"/proc/self"} },
		"token path":       func(s *Service) { s.WritablePaths = []string{"/var/run/secrets/kubernetes.io"} },
		"duplicate path":   func(s *Service) { s.WritablePaths = []string{"/data", "/data"} },
		"nested path":      func(s *Service) { s.WritablePaths = []string{"/data", "/data/pg"} },
		"many paths":       func(s *Service) { s.WritablePaths = []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"} },
		"too many env":     func(s *Service) { s.Env = manyEnv(MaxServiceEnv + 1) },
		"git path segment": func(s *Service) { s.WritablePaths = []string{"/srv/.git"} },
	} {
		s := postgresService()
		change(&s)
		m := withServices(s)
		if m.Validate() == nil || m.Verification.Validate() == nil {
			t.Fatal("accepted", name)
		}
	}
}

func manyEnv(n int) map[string]string {
	env := map[string]string{}
	for i := 0; i < n; i++ {
		env["V"+strings.Repeat("A", i+1)] = "x"
	}
	return env
}

func TestServiceListIsBoundedAndUnique(t *testing.T) {
	services := make([]Service, 0, MaxServices+1)
	for i := 0; i <= MaxServices; i++ {
		s := postgresService()
		s.Name, s.Port = "db"+string(rune('a'+i)), 5432+i
		services = append(services, s)
	}
	if withServices(services[:MaxServices]...).Validate() != nil {
		t.Fatal("MaxServices services refused")
	}
	if withServices(services...).Validate() == nil {
		t.Fatal("more than MaxServices services accepted")
	}
	duplicateName, duplicatePort := services[1], services[1]
	duplicateName.Name, duplicatePort.Port = services[0].Name, services[0].Port
	for name, pair := range map[string][]Service{"name": {services[0], duplicateName}, "port": {services[0], duplicatePort}} {
		if withServices(pair...).Validate() == nil {
			t.Fatal("duplicate service accepted:", name)
		}
	}
	slow := services[1]
	slow.ReadySeconds = 150
	if withServices(services[0], slow).Verification.ServiceReadySeconds() != 150 {
		t.Fatal("the readiness budget is the longest service's")
	}
}

// The quarkus-super-heroes rest-villains example is the reference for a
// repository whose tests need a database: it must parse, keep the Quarkus
// switches in verification.env and run its Postgres non-root.
func TestQuarkusServicesExampleParses(t *testing.T) {
	raw, err := os.ReadFile("../../../../examples/quarkus-super-heroes-rest-villains.project.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := m.Verification
	if len(v.Services) != 1 || v.Services[0].Port != 5432 || v.Services[0].User == 0 || v.Services[0].WritableRootFilesystem ||
		v.Env["QUARKUS_DEVSERVICES_ENABLED"] != "false" ||
		!strings.Contains(v.Env["QUARKUS_DATASOURCE_JDBC_URL"], "localhost:5432") ||
		!v.MatchesTest("rest-villains/src/test/java/io/quarkus/sample/superheroes/villain/VillainTests.java") ||
		!v.MatchesHarness("rest-villains/pom.xml") {
		t.Fatalf("unexpected Quarkus example: %+v", v)
	}
}
