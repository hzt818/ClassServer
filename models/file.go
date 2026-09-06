package models

import "time"

type ChatFile struct {
	ID           int       `json:"id"`
	UploaderID   int       `json:"uploader_id"`
	UploaderName string    `json:"uploader_name"`
	UploaderRole string    `json:"uploader_role"`
	RoomID       int       `json:"room_id"`
	Filename     string    `json:"filename"`
	Filepath     string    `json:"filepath"`
	Filesize     int64     `json:"filesize"`
	AccessType   string    `json:"access_type"` // public, private, group
	TargetUserID *int      `json:"target_user_id,omitempty"`
	IsDeleted    bool      `json:"is_deleted"`
	UploadedAt   time.Time `json:"uploaded_at"`
}
