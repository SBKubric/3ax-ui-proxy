package controller

import "github.com/gin-gonic/gin"

// monitoring renders the Monitoring page (docs/spec/monitoring-panel.md §7.1).
// The route is registered in XUIController.initRouter; the handler lives here
// so xui.go carries a single added line.
func (a *XUIController) monitoring(c *gin.Context) {
	html(c, "monitoring.html", "pages.monitoring.title", nil)
}
