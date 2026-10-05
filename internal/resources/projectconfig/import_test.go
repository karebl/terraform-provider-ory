package projectconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const importProjectDocument = `{
  "id":"proj-1", "name":"example", "slug":"example", "environment":"stage",
  "home_region":"eu-central", "revision_id":"revision-1", "organizations":[], "state":"running",
  "cors_public":{"enabled":false},
  "services":{"identity":{"config":{
    "session":{"lifespan":"20m0s", "cookie":{"same_site":"Lax"}},
    "selfservice":{
      "default_browser_return_url":"https://app.example.com/",
      "allowed_return_urls":["https://app.example.com/"],
      "flows":{"login":{"ui_url":"https://app.example.com/login"}},
      "methods":{"password":{"enabled":true},"totp":{"enabled":false}}
    }
  }}}
}`

func importConfig(t *testing.T, r *ProjectConfigResource, id string) *resource.ImportStateResponse {
	t.Helper()
	ctx := context.Background()
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	require.False(t, schemaResp.Diagnostics.HasError())
	resp := &resource.ImportStateResponse{State: projectConfigState(t, schemaResp.Schema)}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	return resp
}

// Selecting fields must load their actual values without taking ownership of
// unrelated settings. Import must issue no mutating API request.
func TestImportProjectConfig_SelectedScalars(t *testing.T) {
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodGet, req.Method)
		assert.Equal(t, "/projects/proj-1", req.URL.Path)
		reads++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(importProjectDocument))
	}))
	defer srv.Close()

	r := projectConfigResourceForServer(t, srv.URL)
	resp := importConfig(t, r, "proj-1:session_lifespan,cors_enabled,selfservice_methods_password_enabled,selfservice_methods_totp_enabled,selfservice_flows_login_ui_url,selfservice_default_browser_return_url")
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	var state ProjectConfigResourceModel
	require.False(t, resp.State.Get(context.Background(), &state).HasError())
	assert.Equal(t, types.StringValue("proj-1"), state.ID)
	assert.Equal(t, types.StringValue("proj-1"), state.ProjectID)
	assert.Equal(t, types.StringValue("20m0s"), state.SessionLifespan)
	assert.Equal(t, types.BoolValue(false), state.CorsEnabled)
	assert.Equal(t, types.BoolValue(true), state.SelfserviceMethodsPasswordEnabled)
	assert.Equal(t, types.BoolValue(false), state.SelfserviceMethodsTOTPEnabled)
	assert.Equal(t, types.StringValue("https://app.example.com/login"), state.SelfserviceFlowsLoginUIURL)
	assert.Equal(t, types.StringValue("https://app.example.com/"), state.SelfserviceDefaultBrowserReturnURL)
	assert.True(t, state.SessionCookieSameSite.IsNull())
	assert.True(t, state.AllowedReturnURLs.IsNull())
	assert.True(t, state.KetoNamespaces.IsNull())
	assert.True(t, state.SMTPConnectionURI.IsNull())
	assert.Positive(t, reads)
}

func TestImportProjectConfig_LegacyIDDoesNotAcquireFields(t *testing.T) {
	// Legacy import needs no client. The subsequent Read still sees null config.
	resp := importConfig(t, &ProjectConfigResource{}, "proj-1")
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	var state ProjectConfigResourceModel
	require.False(t, resp.State.Get(context.Background(), &state).HasError())
	assert.Equal(t, types.StringValue("proj-1"), state.ProjectID)
	assert.True(t, state.SessionLifespan.IsNull())
	assert.True(t, state.CorsEnabled.IsNull())
}

func TestImportProjectConfig_RejectsUnsafeSelectionBeforeReading(t *testing.T) {
	for _, id := range []string{
		":session_lifespan", "proj-1:", "proj-1:session_lifespan,",
		"proj-1:session_lifespan,session_lifespan", "proj-1:no_such_field",
		"proj-1:id", "proj-1:project_id", "proj-1:smtp_connection_uri",
		"proj-1:smtp_connection_uri_wo", "proj-1:keto_namespaces",
		"proj-1:session_tokenizer_templates",
	} {
		t.Run(id, func(t *testing.T) {
			// A nil client ensures validation happens before any API access.
			resp := importConfig(t, &ProjectConfigResource{}, id)
			assert.True(t, resp.Diagnostics.HasError(), "invalid import must fail: %s", id)
		})
	}
}

func TestImportProjectConfig_DoesNotInventUnreadableValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(importProjectDocument))
	}))
	defer srv.Close()
	// This revision/version counter is local metadata, with no API value to read.
	resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:smtp_connection_uri_wo_version")
	assert.True(t, resp.Diagnostics.HasError(), "unreadable fields must not get fabricated defaults")
}

func TestImportProjectConfig_ReportsReadFailure(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"cannot read project"}}`))
			}))
			defer srv.Close()
			resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:session_lifespan")
			assert.True(t, resp.Diagnostics.HasError(), "failed reads must not import an empty baseline")
		})
	}
}

// Hook readers interpret malformed hook lists as absent hooks. Until those
// readers can distinguish the two, import must not turn that into a false value.
func TestImportProjectConfig_RejectsHookDerivedValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"proj-1","name":"example","slug":"example","environment":"stage","home_region":"eu-central","revision_id":"r","organizations":[],"state":"running","services":{"identity":{"config":{"selfservice":{"flows":{"registration":{"after":{"password":{"hooks":"unreadable"}}}}}}}}}`))
	}))
	defer srv.Close()
	resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:selfservice_flows_registration_after_password_hook_session")
	assert.True(t, resp.Diagnostics.HasError(), "malformed hooks must not import as false")
}
