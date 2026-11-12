package authz_test

import (
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/authz"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

func priv(kind string, props map[string]string) model.Privilege {
	return model.Privilege{Name: "p", Type: kind, Properties: props}
}

func TestPermissionOf(t *testing.T) {
	cases := []struct {
		name string
		p    model.Privilege
		want string // "" when the privilege grants nothing
	}{
		{"wildcard", priv("wildcard", map[string]string{"pattern": "nexus:repository-view:raw:demo-raw:*"}),
			"nexus:repository-view:raw:demo-raw:*"},
		{"wildcard lower cased", priv("wildcard", map[string]string{"pattern": " NEXUS:Settings:Read "}),
			"nexus:settings:read"},
		{"wildcard without pattern", priv("wildcard", map[string]string{}), ""},
		{"wildcard with empty pattern", priv("wildcard", map[string]string{"pattern": ""}), ""},
		{"wildcard that does not parse", priv("wildcard", map[string]string{"pattern": "::"}), ""},
		{"application", priv("application", map[string]string{"domain": "settings", "actions": "read"}),
			"nexus:settings:read"},
		{"application without actions", priv("application", map[string]string{"domain": "settings"}),
			"nexus:settings:*"},
		{"application with empty domain", priv("application", map[string]string{"domain": "", "actions": "read"}),
			"nexus:*:read"},
		{"application with a domain of two parts",
			priv("application", map[string]string{"domain": "atlas:support", "actions": "read"}),
			"nexus:atlas:support:read"},
		{"application actions trimmed",
			priv("application", map[string]string{"domain": "settings", "actions": " read , update ,,"}),
			"nexus:settings:read,update"},
		{"application with only commas as actions",
			priv("application", map[string]string{"domain": "settings", "actions": ",,"}), ""},
		{"repository view", priv("repository-view",
			map[string]string{"format": "maven2", "repository": "demo-maven", "actions": "browse,read"}),
			"nexus:repository-view:maven2:demo-maven:browse,read"},
		{"repository view without properties", priv("repository-view", nil), "nexus:repository-view:*:*:*"},
		{"repository view upper case", priv("repository-view",
			map[string]string{"format": "Maven2", "repository": "Demo-Maven", "actions": "READ"}),
			"nexus:repository-view:maven2:demo-maven:read"},
		{"repository view with a comma in the name", priv("repository-view",
			map[string]string{"format": "maven2", "repository": "demo-maven,demo-raw", "actions": "read"}), ""},
		{"repository view with a colon in the format", priv("repository-view",
			map[string]string{"format": "maven2:x", "repository": "demo-maven", "actions": "read"}), ""},
		{"repository view with a colon in an action", priv("repository-view",
			map[string]string{"format": "maven2", "repository": "demo-maven", "actions": "read:x"}), ""},
		{"repository admin", priv("repository-admin",
			map[string]string{"format": "npm", "repository": "demo-npm", "actions": "*"}),
			"nexus:repository-admin:npm:demo-npm:*"},
		{"content selector grants nothing", priv("repository-content-selector",
			map[string]string{"contentSelector": "demo-selector", "repository": "*-maven2", "actions": "read"}), ""},
		{"script", priv("script", map[string]string{"name": "demo-script", "actions": "browse,read,run"}),
			"nexus:script:demo-script:browse,read,run"},
		{"script with the REST key", priv("script", map[string]string{"scriptName": "demo-script", "actions": "run"}),
			"nexus:script:demo-script:run"},
		{"script without name", priv("script", map[string]string{"actions": "run"}), "nexus:script:*:run"},
		{"unknown kind", priv("demo-kind", map[string]string{"pattern": "nexus:*"}), ""},
	}
	for _, c := range cases {
		got, ok := authz.PermissionOf(c.p)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("%s: PermissionOf = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}
