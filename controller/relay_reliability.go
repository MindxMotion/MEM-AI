package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GetRelayReliability(c *gin.Context) {
	days, daysErr := strconv.Atoi(c.DefaultQuery("days", "7"))
	offset, offsetErr := strconv.Atoi(c.DefaultQuery("offset_minutes", "0"))
	if daysErr != nil || offsetErr != nil || days < 1 || days > 30 || offset < -840 || offset > 840 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	result, err := model.QueryRelayReliability(ctx, time.Now(), days, offset)
	if errors.Is(err, model.ErrReliabilityWindowTooLarge) {
		common.ApiErrorStatus(c, http.StatusUnprocessableEntity, common.NewMessage("Too many observations. Choose a shorter period."))
		return
	}
	if err != nil {
		common.SysError("request reliability query failed: " + err.Error())
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to load request reliability"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}
