package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"recallo/internals/logger"
	"recallo/internals/middleware"
	"recallo/internals/models"
	"recallo/internals/utils"
)

const (
	defaultPage  = 1
	defaultLimit = 20
	maxLimit     = 100
)

func HandleGetPrivate(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())
	privateIDStr := r.PathValue("private_id")
	privateID, err := strconv.ParseInt(privateIDStr, 10, 64)
	if err != nil {
		log.Warn("invalid private_id", "raw_id", privateIDStr, "error", err)
		utils.JSON(w, http.StatusBadRequest, false, "invalid private_id", nil)
		return
	}

	private, err := models.GetPrivateByID(privateID)
	if err != nil {
		log.Error("failed to find private conversation", "private_id", privateID, "error", err)
		utils.JSON(w, http.StatusNotFound, false, "private conversation not found", nil)
		return
	}

	log.Debug("retrieved private conversation", "private_id", privateID)
	utils.JSON(w, http.StatusOK, true, "private conversation retrieved successfully", private)
}

func HandleJoinPrivate(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())
	userID, ok := r.Context().Value(middleware.CtxUserID).(int64)
	if !ok {
		log.Error("missing user_id in request context")
		utils.JSON(w, http.StatusUnauthorized, false, "Unauthorized", nil)
		return
	}

	var req struct {
		ReceiverId int64 `json:"receiver_id"`
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	defer r.Body.Close()
	if err != nil || req.ReceiverId == 0 {
		log.Warn("invalid request body for joining private conversation", "user_id", userID, "error", err)
		utils.JSON(w, http.StatusBadRequest, false, "invalid requested data", nil)
		return
	}

	if userID == req.ReceiverId {
		log.Warn("user attempted to start a conversation with themselves", "user_id", userID)
		utils.JSON(w, http.StatusBadRequest, false, "cannot start a conversation with yourself", nil)
		return
	}

	_, err = models.GetUserByID(req.ReceiverId)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Warn("receiver not found", "receiver_id", req.ReceiverId)
			utils.JSON(w, http.StatusNotFound, false, "receiver not found", nil)
			return
		}
		log.Error("failed to query receiver", "receiver_id", req.ReceiverId, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "internal server error", nil)
		return
	}

	private, err := models.GetPrivateByUsers(userID, req.ReceiverId)
	if err != nil {
		log.Error("failed to query private conversation by users", "user_id: ", userID, "receiver_id: ", req.ReceiverId, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "failed to retrieve private conversation", nil)
		return
	}

	if private != nil {
		log.Debug("existing private conversation found", "user_id", userID, "receiver_id", req.ReceiverId, "private_id", private.ID)
		utils.JSON(w, http.StatusOK, true, "private conversation retrieved successfully", private)
		return
	}

	private, err = models.CreatePrivate(userID, req.ReceiverId)
	if err != nil {
		log.Error("failed to create private conversation", "user_id", userID, "receiver_id", req.ReceiverId, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "failed to create private conversation", nil)
		return
	}

	log.Info("created private conversation", "user_id", userID, "receiver_id", req.ReceiverId, "private_id", private.ID)
	utils.JSON(w, http.StatusCreated, true, "private conversation created successfully", private)
}

func HandleGetConversations(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())
	userID, ok := r.Context().Value(middleware.CtxUserID).(int64)
	if !ok {
		log.Error("missing user_id in request context")
		utils.JSON(w, http.StatusUnauthorized, false, "Unauthorized", nil)
		return
	}

	privates, err := models.GetPrivatesForUser(userID)
	if err != nil {
		log.Error("failed to retrieve conversations from db", "user_id", userID, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "failed to retrieve conversations", nil)
		return
	}

	log.Debug("retrieved conversations list", "user_id", userID, "count", len(privates))
	utils.JSON(w, http.StatusOK, true, "conversations retrieved successfully", privates)
}

func HandleGetPrivateMessages(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())
	privateIdStr := r.PathValue("private_id")

	privateID, err := strconv.ParseInt(privateIdStr, 10, 64)
	if err != nil {
		log.Warn("invalid private_id in message retrieval", "raw_id", privateIdStr, "error", err)
		utils.JSON(w, http.StatusBadRequest, false, "Invalid private id", nil)
		return
	}

	page := defaultPage
	limit := defaultLimit

	if p := r.URL.Query().Get("page"); p != "" {
		var parseErr error
		page, parseErr = strconv.Atoi(p)
		if parseErr != nil || page < 1 {
			log.Warn("invalid page number", "raw_page", p, "private_id", privateID, "error", parseErr)
			utils.JSON(w, http.StatusBadRequest, false, "invalid page number", nil)
			return
		}
	}

	if l := r.URL.Query().Get("limit"); l != "" {
		var parseErr error
		limit, parseErr = strconv.Atoi(l)
		if parseErr != nil || limit < 1 || limit > maxLimit {
			log.Warn("invalid limit number", "raw_limit", l, "private_id", privateID, "error", parseErr)
			utils.JSON(w, http.StatusBadRequest, false, "invalid limit number (must be between 1 and 100)", nil)
			return
		}
	}

	log.Debug("fetching private messages", "private_id", privateID, "page", page, "limit", limit)

	messages, err := models.GetMessagesByPrivateID(privateID, page, limit+1)
	if err != nil {
		log.Error("failed to retrieve messages from database", "private_id", privateID, "page", page, "limit", limit, "error", err)
		utils.JSON(w, http.StatusInternalServerError, false, "failed to retrieve messages", nil)
		return
	}

	hasNextPage := false
	if len(messages) > limit {
		hasNextPage = true
		messages = messages[:limit]
	}

	log.Debug("successfully retrieved messages", "private_id", privateID, "page", page, "limit", limit, "count", len(messages), "has_next", hasNextPage)
	utils.JSON(w, http.StatusOK, true, "messages retrieved successfully", map[string]any{
		"messages":      messages,
		"page":          page,
		"limit":         limit,
		"has_next_page": hasNextPage,
	})
}
