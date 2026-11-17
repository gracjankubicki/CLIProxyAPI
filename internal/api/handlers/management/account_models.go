package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmodels"
	"net/http"
)

func (h *Handler) SetAccountModelDiscovery(refresh, status func() accountmodels.Status) {
	h.accountModelsRefresh = refresh
	h.accountModelsStatus = status
}

func (h *Handler) GetAccountModels(c *gin.Context) {
	if h.accountModelsStatus == nil {
		c.JSON(http.StatusOK, accountmodels.Status{Accounts: []accountmodels.Snapshot{}})
		return
	}
	c.JSON(http.StatusOK, h.accountModelsStatus())
}

func (h *Handler) RefreshAccountModels(c *gin.Context) {
	if h.accountModelsRefresh == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account model discovery unavailable"})
		return
	}
	s := h.accountModelsRefresh()
	if !s.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "enable routing.account-model-discovery.enabled first"})
		return
	}
	c.JSON(http.StatusAccepted, s)
}
