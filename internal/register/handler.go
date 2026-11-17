package register

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler 暴露注册机的 admin 接口，路径与 dev 的 api/register.py 保持一致。
type Handler struct{ svc *Service }

// NewHandler 构造注册机 handler。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterRoutes 挂载到 admin 分组下。
func (h *Handler) RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/register", h.get)
	router.POST("/register", h.update)
	router.POST("/register/start", h.start)
	router.POST("/register/stop", h.stop)
	router.POST("/register/reset", h.reset)
	router.POST("/register/outlook-pool/reset", h.resetOutlookPool)
	router.GET("/register/events", h.events)
}

func (h *Handler) get(c *gin.Context) { c.JSON(http.StatusOK, h.svc.Snapshot()) }

func (h *Handler) update(c *gin.Context) {
	var patch UpdateRequest
	if err := c.ShouldBindJSON(&patch); err != nil {
		writeRegisterError(c, http.StatusBadRequest, "注册配置格式不正确: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, h.svc.Update(patch))
}

func (h *Handler) start(c *gin.Context) { c.JSON(http.StatusOK, h.svc.Start()) }

func (h *Handler) stop(c *gin.Context) { c.JSON(http.StatusOK, h.svc.Stop()) }

func (h *Handler) reset(c *gin.Context) { c.JSON(http.StatusOK, h.svc.Reset()) }

func (h *Handler) resetOutlookPool(c *gin.Context) {
	var body struct {
		Scope string `json:"scope"`
	}
	_ = c.ShouldBindJSON(&body)
	c.JSON(http.StatusOK, h.svc.ResetOutlookPool(body.Scope))
}

// events 提供 SSE 推送：0.5s 对比一次快照，内容变化才推送。
func (h *Handler) events(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	last := ""
	c.Stream(func(w io.Writer) bool {
		select {
		case <-c.Request.Context().Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
		payload, err := json.Marshal(h.svc.Snapshot())
		if err != nil {
			return true
		}
		current := string(payload)
		if current == last {
			return true
		}
		last = current
		_, _ = io.WriteString(w, "data: "+strings.ReplaceAll(current, "\n", "")+"\n\n")
		return true
	})
}

func writeRegisterError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"detail": gin.H{"error": message}})
}
