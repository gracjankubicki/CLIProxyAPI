package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmodels"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountModelRefreshIsAsyncAndDisabledIsExplicit(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	h.RefreshAccountModels(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal(rec.Code)
	}
	h.SetAccountModelDiscovery(func() accountmodels.Status { return accountmodels.Status{} }, nil)
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	h.RefreshAccountModels(c)
	if rec.Code != http.StatusConflict {
		t.Fatal(rec.Code)
	}
	h.SetAccountModelDiscovery(func() accountmodels.Status { return accountmodels.Status{Enabled: true, Running: true} }, func() accountmodels.Status { return accountmodels.Status{Enabled: true, Running: true} })
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	h.RefreshAccountModels(c)
	if rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	h.GetAccountModels(c)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
}
