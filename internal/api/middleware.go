package api

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const ctxUserID = "userID"

// requireAuth rejects requests without a valid Bearer token and stores the
// resolved user ID in the context for handlers.
func (s *Server) requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Request().Header.Get(echo.HeaderAuthorization)
		if !strings.HasPrefix(h, "Bearer ") {
			return echo.NewHTTPError(http.StatusUnauthorized, "missing bearer token")
		}
		uid, err := s.token.Parse(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
		c.Set(ctxUserID, uid)
		return next(c)
	}
}
