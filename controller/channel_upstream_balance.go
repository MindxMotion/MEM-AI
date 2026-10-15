package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func GetUpstreamBalance(c *gin.Context)     { upstreamBalanceResponse(c, false) }
func RefreshUpstreamBalance(c *gin.Context) { upstreamBalanceResponse(c, true) }

func upstreamBalanceResponse(c *gin.Context, refresh bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid parameters"})
		return
	}
	channel, err := model.GetChannelById(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Channel not found"})
		return
	}
	profile, err := service.ReadUpstreamBalanceSettings(channel.OtherSettings)
	if err != nil || service.ValidateUpstreamBalanceProfile(profile) != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Invalid upstream balance configuration"})
		return
	}
	if profile == nil || channel.ChannelInfo.IsMultiKey || channel.Type == constant.ChannelTypeTaskPlugin || channel.Key == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Unsupported upstream balance channel"})
		return
	}
	started := time.Now()
	snapshot := service.UpstreamBalance(c.Request.Context(), *profile, channel.Key, refresh)
	if refresh {
		category := snapshot.Error
		if category == "" {
			category = snapshot.Status
		}
		common.SysLog(fmt.Sprintf("Upstream balance refresh channel_id=%d category=%s duration_ms=%d",
			channel.Id, category, time.Since(started).Milliseconds()))
	}
	// Recheck before publishing an in-flight response after a channel edit.
	current, err := model.GetChannelById(id, true)
	if err != nil || current.Key != channel.Key || current.OtherSettings != channel.OtherSettings ||
		current.Type != channel.Type || current.ChannelInfo.IsMultiKey != channel.ChannelInfo.IsMultiKey {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Upstream balance configuration changed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": snapshot})
}
