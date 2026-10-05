package projectconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
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
	resp := &resource.ImportStateResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}}
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
	var values map[string]tftypes.Value
	require.NoError(t, resp.State.Raw.As(&values))
	for name, value := range values {
		switch name {
		case "id", "project_id", "session_lifespan", "cors_enabled", "selfservice_methods_password_enabled", "selfservice_methods_totp_enabled", "selfservice_flows_login_ui_url", "selfservice_default_browser_return_url":
		default:
			assert.True(t, value.IsNull(), "unselected field %s must remain unmanaged", name)
		}
	}
	refreshed := resource.ReadResponse{State: resp.State}
	r.Read(context.Background(), resource.ReadRequest{State: resp.State}, &refreshed)
	require.False(t, refreshed.Diagnostics.HasError(), "%v", refreshed.Diagnostics)
	assert.True(t, resp.State.Raw.Equal(refreshed.State.Raw), "refresh must preserve the imported baseline")
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
	assert.NotEmpty(t, resp.Diagnostics.Warnings())
}

func TestImportProjectConfig_RejectsCourierStorageURL(t *testing.T) {
	document := strings.Replace(importProjectDocument, `"session":`, `"courier":{"http":{"request_config":{"body":"`+storageURL(jsonnetPayload)+`"}}},"session":`, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodGet, req.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(document))
	}))
	defer srv.Close()
	resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:courier_http_request_config_body")
	assert.True(t, resp.Diagnostics.HasError(), "a storage URL must not stand in for the inline courier body")
}

func TestImportProjectConfig_WarnsForUnselectedCORS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodGet, req.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Replace(importProjectDocument, `"enabled":false`, `"enabled":true`, 1)))
	}))
	defer srv.Close()
	for _, selection := range []string{"session_lifespan", "session_lifespan,cors_enabled"} {
		t.Run(selection, func(t *testing.T) {
			resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:"+selection)
			require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
			if selection == "session_lifespan" {
				require.NotEmpty(t, resp.Diagnostics.Warnings(), "omitting CORS needs guidance about its default")
				assert.Contains(t, resp.Diagnostics.Warnings()[0].Detail(), "cors_enabled")
			} else {
				assert.Empty(t, resp.Diagnostics.Warnings(), "selected CORS must not trigger omission guidance")
			}
		})
	}
}

func TestImportProjectConfig_DefaultReturnURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodGet, req.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(importProjectDocument))
	}))
	defer srv.Close()
	r := projectConfigResourceForServer(t, srv.URL)
	resp := importConfig(t, r, "proj-1:default_return_url")
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	var state ProjectConfigResourceModel
	require.False(t, resp.State.Get(context.Background(), &state).HasError())
	assert.Equal(t, types.StringValue("https://app.example.com/"), state.DefaultReturnURL)
	assert.True(t, state.SelfserviceDefaultBrowserReturnURL.IsNull())

	// The unknown import value must not share the existing empty sentinel's
	// behavior: an explicit empty value still means the URL was cleared.
	require.False(t, resp.State.SetAttribute(context.Background(), path.Root("default_return_url"), types.StringValue("")).HasError())
	refreshed := resource.ReadResponse{State: resp.State}
	r.Read(context.Background(), resource.ReadRequest{State: resp.State}, &refreshed)
	require.False(t, refreshed.Diagnostics.HasError(), "%v", refreshed.Diagnostics)
	require.False(t, refreshed.State.Get(context.Background(), &state).HasError())
	assert.Equal(t, types.StringValue(""), state.DefaultReturnURL)
}

func TestImportProjectConfig_IntegerAndDeprecatedAlias(t *testing.T) {
	for _, field := range []string{"selfservice_methods_password_config_min_password_length", "password_min_length"} {
		for _, value := range []string{"12", "0"} {
			t.Run(field+"="+value, func(t *testing.T) {
				document := strings.Replace(importProjectDocument, `"password":{"enabled":true}`, `"password":{"enabled":true,"config":{"min_password_length":`+value+`}}`, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					assert.Equal(t, http.MethodGet, req.Method)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(document))
				}))
				defer srv.Close()
				resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:"+field)
				require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
				var imported types.Int64
				require.False(t, resp.State.GetAttribute(context.Background(), path.Root(field), &imported).HasError())
				want := int64(12)
				if value == "0" {
					want = 0
				}
				assert.Equal(t, types.Int64Value(want), imported)
				var state ProjectConfigResourceModel
				require.False(t, resp.State.Get(context.Background(), &state).HasError())
				if field == "password_min_length" {
					assert.True(t, state.SelfserviceMethodsPasswordConfigMinPasswordLength.IsNull())
				} else {
					assert.True(t, state.PasswordMinLength.IsNull())
				}
			})
		}
	}
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
	for _, field := range []string{
		"smtp_connection_uri_wo_version",                          // Local metadata, with no API value.
		"selfservice_methods_totp_config_issuer",                  // Readable string, absent from the response.
		"selfservice_methods_password_config_min_password_length", // Readable integer, absent from the response.
	} {
		t.Run(field, func(t *testing.T) {
			resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:session_lifespan,"+field)
			assert.True(t, resp.Diagnostics.HasError(), "one unavailable field must fail the import without inventing a default")
		})
	}
}

func TestImportProjectConfig_NormalizedRevision(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			revisionReads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assert.Equal(t, http.MethodGet, req.Method)
				w.Header().Set("Content-Type", "application/json")
				switch req.URL.Path {
				case "/projects/proj-1":
					_, _ = w.Write([]byte(importProjectDocument))
				case "/normalized/projects/proj-1":
					revisionReads++
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = w.Write([]byte(`{"id":"proj-1","current_revision":{"disable_account_experience_welcome_screen":false}}`))
					} else {
						_, _ = w.Write([]byte(`{"error":{"message":"revision unavailable"}}`))
					}
				default:
					t.Errorf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			resp := importConfig(t, projectConfigResourceForServer(t, srv.URL), "proj-1:disable_account_experience_welcome_screen")
			assert.Positive(t, revisionReads)
			if status != http.StatusOK {
				assert.True(t, resp.Diagnostics.HasError(), "revision read failures must fail import")
				return
			}
			require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
			var state ProjectConfigResourceModel
			require.False(t, resp.State.Get(context.Background(), &state).HasError())
			assert.Equal(t, types.BoolValue(false), state.DisableAccountExperienceWelcomeScreen)
		})
	}
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
