package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasswordLoginAdminOnly(t *testing.T) {
	for _, test := range []struct {
		name, policy, password string
		role, status           int
		enabled, factor, allow bool
	}{
		{"ordinary member", "true", "enrollment-password", 1, 1, true, false, false},
		{"ordinary member with MFA", "true", "enrollment-password", 1, 1, true, true, false},
		{"administrator", "true", "enrollment-password", 10, 1, true, false, true},
		{"super administrator", "true", "enrollment-password", 100, 1, true, false, true},
		{"administrator with MFA", "true", "enrollment-password", 10, 1, true, true, true},
		{"incorrect administrator password", "true", "incorrect-password", 100, 1, true, false, false},
		{"disabled administrator", "true", "enrollment-password", 100, 2, true, false, false},
		{"global password switch off", "true", "enrollment-password", 100, 1, false, false, false},
		{"explicit compatibility mode", "false", "enrollment-password", 1, 1, true, false, true},
		{"default compatibility mode", "", "enrollment-password", 1, 1, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			user, _ := setupSecurityEnrollmentTest(t)
			t.Setenv("PASSWORD_LOGIN_ADMIN_ONLY", test.policy)
			previous := common.PasswordLoginEnabled
			common.PasswordLoginEnabled = test.enabled
			t.Cleanup(func() { common.PasswordLoginEnabled = previous })
			require.NoError(t, model.DB.Model(user).Updates(map[string]any{
				"role": test.role, "status": test.status,
			}).Error)
			if test.factor {
				require.NoError(t, model.DB.Create(&model.TwoFA{
					UserId: user.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true,
				}).Error)
			}
			body, err := common.Marshal(map[string]string{
				"username": user.Username, "password": test.password,
			})
			require.NoError(t, err)
			router := gin.New()
			router.POST("/api/user/login", Login)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/user/login", strings.NewReader(string(body))))
			var result struct {
				Success bool `json:"success"`
				Data    struct {
					AccessToken         string `json:"access_token"`
					RequireVerification bool   `json:"require_verification"`
					FlowToken           string `json:"flow_token"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.Equal(t, test.allow, result.Success)
			if !test.allow {
				assert.Empty(t, result.Data.AccessToken)
				assert.Empty(t, result.Data.FlowToken)
				assert.Empty(t, response.Header().Values("Set-Cookie"))
			} else if test.factor {
				assert.True(t, result.Data.RequireVerification)
				assert.NotEmpty(t, result.Data.FlowToken)
				assert.Empty(t, result.Data.AccessToken)
				assert.Empty(t, response.Header().Values("Set-Cookie"))
			} else {
				assert.NotEmpty(t, result.Data.AccessToken)
				assert.NotEmpty(t, response.Header().Values("Set-Cookie"))
			}
			count, err := model.CountActiveUserSessions(user.Id, time.Now().Unix())
			require.NoError(t, err)
			expected := int64(1)
			if test.allow && !test.factor {
				expected++
			}
			assert.Equal(t, expected, count)
		})
	}
}

func TestAdminOnlyPasswordPolicyDoesNotBlockOrdinarySSO(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	t.Setenv("PASSWORD_LOGIN_ADMIN_ONLY", "true")
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/oidc", nil)
	c.Set("login_method", "oauth:oidc")
	setupLogin(user, nil, c)
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	identity, err := service.ParseAccessToken(result.Data.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, user.Id, identity.UserID)
}
