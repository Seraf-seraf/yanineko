package handler

import (
	"net/http"

	vercelruntime "github.com/Seraf-seraf/yanineko/platform/vercel"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	vercelruntime.Handler(w, r)
}
