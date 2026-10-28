package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func setIntegrationRouter(api *gin.RouterGroup) {
	integration := api.Group("/integration/v1", middleware.DisableCache())
	integration.POST("/token", middleware.IntegrationTokenRateLimit(), controller.IntegrationToken)
	integration.GET("/balance", middleware.IntegrationBalanceAuth(), controller.GetIntegrationBalance)

	clients := api.Group("/integration-clients", middleware.DisableCache(), middleware.RootAuth())
	clients.GET("", controller.ListIntegrationClients)
	clients.POST("", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("integration-client"), controller.CreateIntegrationClient)
	clients.POST("/:client_id/rotate", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("integration-client"), controller.RotateIntegrationClient)
	clients.PATCH("/:client_id", middleware.CriticalRateLimit(), middleware.UserCriticalRateLimit("integration-client"), controller.SetIntegrationClientStatus)
}
