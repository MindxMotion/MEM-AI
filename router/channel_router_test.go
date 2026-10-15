package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelDefaultBaseURLsRequireReadPermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/default_base_urls", authz.ChannelRead, controller.GetChannelDefaultBaseURLs)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/channel/default_base_urls", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestChannelStatusRoutesUseExpectedPermissions(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/:id/vllm/status", authz.ChannelRead, controller.GetVLLMChannelStatus)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/sglang/status", authz.ChannelRead, controller.GetSGLangChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.UpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.DeleteChannel)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.DeleteChannelBatch)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.DeleteDisabledChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.EditTagChannels)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.BatchSetChannelTag)
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

func TestUpstreamBalanceRoutesRejectAnonymousRequests(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/:id/upstream_balance", authz.ChannelRead, controller.GetUpstreamBalance)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/upstream_balance/refresh", authz.ChannelOperate, controller.RefreshUpstreamBalance)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/channel/11/upstream_balance"},
		{http.MethodPost, "/api/channel/11/upstream_balance/refresh"},
	} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "total")
		assert.NotContains(t, recorder.Body.String(), "fixture-key")
	}
}

func TestUpstreamBalanceRoutesEnforceRoleScopeAndPermission(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousRedis, previousMaster := common.MainDatabaseType(), common.RedisEnabled, common.IsMasterNode
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.AuditLog{}, &model.UserAccessToken{},
		&model.Option{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.Channel{}))
	model.DB, model.LOG_DB = db, db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RedisEnabled, common.IsMasterNode = false, true
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled, common.IsMasterNode = previousRedis, previousMaster
	})
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	for _, tc := range []struct {
		name, scope, method string
		role                int
		revoke              bool
	}{
		{"ordinary user cannot read", "channel:read", http.MethodGet, common.RoleCommonUser, false},
		{"ordinary user cannot refresh", "channel:operate", http.MethodPost, common.RoleCommonUser, false},
		{"admin missing read scope", "profile:read", http.MethodGet, common.RoleAdminUser, false},
		{"read scope cannot refresh", "channel:read", http.MethodPost, common.RoleAdminUser, false},
		{"operate scope cannot read", "channel:operate", http.MethodGet, common.RoleAdminUser, false},
		{"scope cannot widen revoked account permission", "channel:read", http.MethodGet, common.RoleAdminUser, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := &model.User{Username: tc.name, Password: "fixture-password", Role: tc.role,
				Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: tc.name}
			require.NoError(t, db.Create(user).Error)
			suffix, err := common.GenerateRandomCharsKey(43)
			require.NoError(t, err)
			raw := model.AccessTokenPrefix + suffix
			token := &model.UserAccessToken{Name: "fixture", TokenHash: model.AccessTokenFingerprint(raw),
				TokenHint: model.AccessTokenHint(raw), ExpiresAt: time.Now().Unix() + 3600}
			require.NoError(t, token.SetScopes([]string{tc.scope}))
			require.NoError(t, model.CreateUserAccessToken(user.Id, token, service.AccessTokenMaxPerUser))
			if tc.revoke {
				require.NoError(t, authz.SetUserPermissions(user.Id, authz.PermissionsMap{authz.ResourceChannel: {authz.ActionRead: false}}))
			}
			path := "/api/channel/11/upstream_balance"
			if tc.method == http.MethodPost {
				path += "/refresh"
			}
			request := httptest.NewRequest(tc.method, path, nil)
			request.Header.Set("Authorization", "Bearer "+raw)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.NotContains(t, response.Body.String(), raw)
			assert.NotContains(t, response.Body.String(), `"total"`)
		})
	}
	t.Run("SQLite settings preserve credentials and local accounting", func(t *testing.T) {
		for _, id := range []int{11, 12} {
			channel := model.Channel{Id: id, Type: 1, Key: "shared-fixture-key", Name: "fixture",
				Status: common.ChannelStatusEnabled, UsedQuota: 1234, Balance: 7.25,
				OtherSettings: `{"preserved":{"keep":true}}`}
			require.NoError(t, db.Create(&channel).Error)
			config := `{"preserved":{"keep":true},"upstream_balance":{"profile":"legacy_subscription_usage","origin":"primary","unit":"USD","scope":"account","period":"cumulative"}}`
			require.NoError(t, db.Model(&channel).Update("settings", config).Error)
			var saved model.Channel
			require.NoError(t, db.First(&saved, id).Error)
			assert.Equal(t, config, saved.OtherSettings)
			assert.Equal(t, channel.Key, saved.Key)
			assert.Equal(t, channel.UsedQuota, saved.UsedQuota)
			assert.Equal(t, channel.Balance, saved.Balance)
			assert.Equal(t, channel.Status, saved.Status)
		}
		var tokens int64
		require.NoError(t, db.Model(&model.UserAccessToken{}).Count(&tokens).Error)
		assert.EqualValues(t, 6, tokens, "settings writes do not replace fixture tokens")
		var version string
		require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
		t.Logf("Real SQLite settings round-trip engine=%s", version)
	})
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}
