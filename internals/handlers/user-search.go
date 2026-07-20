package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"recallo/internals/cache"
	"recallo/internals/logger"
	"recallo/internals/middleware"
	"recallo/internals/models"
	"recallo/internals/utils"
)

const mutualSearchTTL = 300 * time.Second

func HandleSearchUsers(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())

	userID, ok := r.Context().Value(middleware.CtxUserID).(int64)
	if !ok {
		utils.JSON(w, http.StatusUnauthorized, false, "Unauthorized", nil)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		utils.JSON(w, http.StatusOK, true, "no query", []*models.UserSearchResult{})
		return
	}

	ctx := r.Context()
	cacheKey := "mutual_search:" + strconv.FormatInt(userID, 10) + ":" + query

	// Cache hit — return the stored JSON payload directly.
	if cached, hit := cache.Get(ctx, cacheKey); hit {
		var results []*models.UserSearchResult
		if err := json.Unmarshal([]byte(cached), &results); err == nil {
			utils.JSON(w, http.StatusOK, true, "users retrieved (cached)", results)
			return
		}
	}

	// Cache miss — hit the database.
	results, err := models.SearchMutualUsers(userID, query)
	if err != nil {
		log.Error("mutual user search failed", "user_id", userID, "query", query, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "failed to search users", nil)
		return
	}

	// Best-effort cache write; ignore failures so search still succeeds.
	if payload, err := json.Marshal(results); err == nil {
		_ = cache.Set(ctx, cacheKey, string(payload), mutualSearchTTL)
	}

	utils.JSON(w, http.StatusOK, true, "users retrieved successfully", results)
}
