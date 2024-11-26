package main

import (
	"fmt"
	"net/http"
)

type Error struct {
	Message string `json:"message"`
}

func sendError(w http.ResponseWriter, code int, msg string, args ...any) {
	w.WriteHeader(code)
	sendJSON(w, Error{Message: fmt.Sprintf(msg, args...)})
}
