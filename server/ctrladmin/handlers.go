package ctrladmin

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"senan.xyz/g/gonic/db"
	"senan.xyz/g/gonic/scanner"
	"senan.xyz/g/gonic/server/lastfm"
)

func (c *Controller) ServeNotFound(r *http.Request) *Response {
	return &Response{template: "not_found.tmpl", code: 404}
}

func (c *Controller) ServeLogin(r *http.Request) *Response {
	return &Response{template: "login.tmpl"}
}

func (c *Controller) ServeHome(r *http.Request) *Response {
	data := &templateData{}
	// ** begin stats box
	c.DB.Table("artists").Count(&data.ArtistCount)
	c.DB.Table("albums").Count(&data.AlbumCount)
	c.DB.Table("tracks").Count(&data.TrackCount)
	// ** begin lastfm box
	scheme := firstExisting(
		"http", // fallback
		r.Header.Get("X-Forwarded-Proto"),
		r.Header.Get("X-Forwarded-Scheme"),
		r.URL.Scheme,
	)
	host := firstExisting(
		"localhost:4747", // fallback
		r.Header.Get("X-Forwarded-Host"),
		r.Host,
	)
	data.RequestRoot = fmt.Sprintf("%s://%s", scheme, host)
	data.CurrentLastFMAPIKey = c.DB.GetSetting("lastfm_api_key")
	// ** begin users box
	c.DB.Find(&data.AllUsers)
	// ** begin recent folders box
	c.DB.
		Where("tag_artist_id IS NOT NULL").
		Order("modified_at DESC").
		Limit(8).
		Find(&data.RecentFolders)
	data.IsScanning = scanner.IsScanning()
	if tStr := c.DB.GetSetting("last_scan_time"); tStr != "" {
		i, _ := strconv.ParseInt(tStr, 10, 64)
		data.LastScanTime = time.Unix(i, 0)
	}
	// ** begin playlists box
	user := r.Context().Value(CtxUser).(*db.User)
	c.DB.
		Where("user_id=?", user.ID).
		Limit(20).
		Find(&data.Playlists)
	//
	return &Response{
		template: "home.tmpl",
		data:     data,
	}
}

func (c *Controller) ServeChangeOwnPassword(r *http.Request) *Response {
	return &Response{template: "change_own_password.tmpl"}
}

func (c *Controller) ServeChangeOwnPasswordDo(r *http.Request) *Response {
	passwordOne := r.FormValue("password_one")
	passwordTwo := r.FormValue("password_two")
	err := validatePasswords(passwordOne, passwordTwo)
	if err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{err.Error()},
		}
	}
	user := r.Context().Value(CtxUser).(*db.User)
	user.Password = passwordOne
	c.DB.Save(user)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeLinkLastFMDo(r *http.Request) *Response {
	token := r.URL.Query().Get("token")
	if token == "" {
		return &Response{
			err:  "please provide a token",
			code: 400,
		}
	}
	sessionKey, err := lastfm.GetSession(
		c.DB.GetSetting("lastfm_api_key"),
		c.DB.GetSetting("lastfm_secret"),
		token,
	)
	if err != nil {
		return &Response{
			redirect: "/admin/home",
			flashW:   []string{err.Error()},
		}
	}
	user := r.Context().Value(CtxUser).(*db.User)
	user.LastFMSession = sessionKey
	c.DB.Save(&user)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeUnlinkLastFMDo(r *http.Request) *Response {
	user := r.Context().Value(CtxUser).(*db.User)
	user.LastFMSession = ""
	c.DB.Save(&user)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeChangePassword(r *http.Request) *Response {
	username := r.URL.Query().Get("user")
	if username == "" {
		return &Response{
			err:  "please provide a username",
			code: 400,
		}
	}
	user := c.DB.GetUserFromName(username)
	if user == nil {
		return &Response{
			err:  "couldn't find a user with that name",
			code: 400,
		}
	}
	data := &templateData{}
	data.SelectedUser = user
	return &Response{
		template: "change_password.tmpl",
		data:     data,
	}
}

func (c *Controller) ServeChangePasswordDo(r *http.Request) *Response {
	username := r.URL.Query().Get("user")
	passwordOne := r.FormValue("password_one")
	passwordTwo := r.FormValue("password_two")
	err := validatePasswords(passwordOne, passwordTwo)
	if err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{err.Error()},
		}
	}
	user := c.DB.GetUserFromName(username)
	user.Password = passwordOne
	c.DB.Save(user)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeDeleteUser(r *http.Request) *Response {
	username := r.URL.Query().Get("user")
	if username == "" {
		return &Response{
			err:  "please provide a username",
			code: 400,
		}
	}
	user := c.DB.GetUserFromName(username)
	if user == nil {
		return &Response{
			err:  "couldn't find a user with that name",
			code: 400,
		}
	}
	data := &templateData{}
	data.SelectedUser = user
	return &Response{
		template: "delete_user.tmpl",
		data:     data,
	}
}

func (c *Controller) ServeDeleteUserDo(r *http.Request) *Response {
	username := r.URL.Query().Get("user")
	user := c.DB.GetUserFromName(username)
	if user.IsAdmin {
		return &Response{
			redirect: "/admin/home",
			flashW:   []string{"can't delete the admin user"},
		}
	}
	c.DB.Delete(user)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeCreateUser(r *http.Request) *Response {
	return &Response{template: "create_user.tmpl"}
}

func (c *Controller) ServeCreateUserDo(r *http.Request) *Response {
	username := r.FormValue("username")
	err := validateUsername(username)
	if err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{err.Error()},
		}
	}
	passwordOne := r.FormValue("password_one")
	passwordTwo := r.FormValue("password_two")
	err = validatePasswords(passwordOne, passwordTwo)
	if err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{err.Error()},
		}
	}
	user := db.User{
		Name:     username,
		Password: passwordOne,
	}
	err = c.DB.Create(&user).Error
	if err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{fmt.Sprintf("could not create user `%s`: %v", username, err)},
		}
	}
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeUpdateLastFMAPIKey(r *http.Request) *Response {
	data := &templateData{}
	data.CurrentLastFMAPIKey = c.DB.GetSetting("lastfm_api_key")
	data.CurrentLastFMAPISecret = c.DB.GetSetting("lastfm_secret")
	return &Response{
		template: "update_lastfm_api_key.tmpl",
		data:     data,
	}
}

func (c *Controller) ServeUpdateLastFMAPIKeyDo(r *http.Request) *Response {
	apiKey := r.FormValue("api_key")
	secret := r.FormValue("secret")
	if err := validateAPIKey(apiKey, secret); err != nil {
		return &Response{
			redirect: r.Referer(),
			flashW:   []string{err.Error()},
		}
	}
	c.DB.SetSetting("lastfm_api_key", apiKey)
	c.DB.SetSetting("lastfm_secret", secret)
	return &Response{redirect: "/admin/home"}
}

func (c *Controller) ServeStartScanDo(r *http.Request) *Response {
	defer func() {
		go func() {
			if err := c.Scanner.Start(); err != nil {
				log.Printf("error while scanning: %v\n", err)
			}
		}()
	}()
	return &Response{
		redirect: "/admin/home",
		flashN:   []string{"scan started. refresh for results"},
	}
}

func (c *Controller) ServeUploadPlaylist(r *http.Request) *Response {
	return &Response{template: "upload_playlist.tmpl"}
}

func (c *Controller) ServeUploadPlaylistDo(r *http.Request) *Response {
	if err := r.ParseMultipartForm((1 << 10) * 24); nil != err {
		return &Response{
			err:  "couldn't parse mutlipart",
			code: 500,
		}
	}
	user := r.Context().Value(CtxUser).(*db.User)
	var playlistCount int
	var errors []string
	for _, headers := range r.MultipartForm.File {
		for _, header := range headers {
			headerErrors, created := playlistParseUpload(c, user.ID, header)
			if created {
				playlistCount++
			}
			errors = append(errors, headerErrors...)
		}
	}
	return &Response{
		redirect: "/admin/home",
		flashN:   []string{fmt.Sprintf("%d playlist(s) created", playlistCount)},
		flashW:   errors,
	}
}
