package scanner

import (
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/karrick/godirwalk"
	"github.com/pkg/errors"
	"github.com/rainycape/unidecode"

	"senan.xyz/g/gonic/db"
	"senan.xyz/g/gonic/mime"
	"senan.xyz/g/gonic/scanner/stack"
	"senan.xyz/g/gonic/scanner/tags"
)

// isScanning acts as an atomic boolean semaphore. we don't
// want to have more than one scan going on at a time
var isScanning int32

func IsScanning() bool {
	return atomic.LoadInt32(&isScanning) == 1
}

func SetScanning() func() {
	atomic.StoreInt32(&isScanning, 1)
	return func() {
		atomic.StoreInt32(&isScanning, 0)
	}
}

type Scanner struct {
	db        *db.DB
	musicPath string
	// these two are for the transaction we do for every folder.
	// the boolean is there so we dont begin or commit multiple
	// times in the handle folder or post children callback
	trTx     *gorm.DB
	trTxOpen bool
	// these two are for keeping state between noted in the tree.
	// eg. keep track of a parents folder or the path to a cover
	// we just saw that we need to commit in the post children
	// callback
	curFolders *stack.Stack
	curCover   string
	// then the rest are for stats and cleanup at the very end
	seenTracks    map[int]struct{} // set of p keys
	seenFolders   map[int]struct{} // set of p keys
	seenTracksNew int              // n tracks not seen before
	seenTracksErr int              // n tracks we we couldn't scan
}

func New(db *db.DB, musicPath string) *Scanner {
	return &Scanner{
		db:          db,
		musicPath:   musicPath,
		seenTracks:  make(map[int]struct{}),
		seenFolders: make(map[int]struct{}),
		curFolders:  &stack.Stack{},
	}
}

func (s *Scanner) Start() error {
	if IsScanning() {
		return errors.New("already scanning")
	}
	unSet := SetScanning()
	defer unSet()
	// reset tracking variables when finished
	defer func() {
		s.seenTracks = make(map[int]struct{})
		s.seenFolders = make(map[int]struct{})
		s.curFolders = &stack.Stack{}
		s.seenTracksNew = 0
		s.seenTracksErr = 0
	}()
	// ** begin being walking
	start := time.Now()
	err := godirwalk.Walk(s.musicPath, &godirwalk.Options{
		Callback:             s.callbackItem,
		PostChildrenCallback: s.callbackPost,
		Unsorted:             true,
		FollowSymbolicLinks:  true,
	})
	if err != nil {
		return errors.Wrap(err, "walking filesystem")
	}
	log.Printf("finished scan in %s, +%d/%d tracks (%d err)\n",
		time.Since(start),
		s.seenTracksNew,
		len(s.seenTracks),
		s.seenTracksErr,
	)
	// ** begin cleaning
	start = time.Now()
	var deleted uint
	// delete tracks not on filesystem
	s.db.WithTx(func(tx *gorm.DB) {
		var tracks []*db.Track
		tx.Select("id").Find(&tracks)
		for _, track := range tracks {
			if _, ok := s.seenTracks[track.ID]; !ok {
				tx.Delete(track)
				deleted++
			}
		}
	})
	// delete folders not on filesystem
	s.db.WithTx(func(tx *gorm.DB) {
		var folders []*db.Album
		tx.Select("id").Find(&folders)
		for _, folder := range folders {
			if _, ok := s.seenFolders[folder.ID]; !ok {
				tx.Delete(folder)
			}
		}
	})
	// delete albums without tracks
	s.db.Exec(`
		DELETE FROM albums
		WHERE tag_artist_id NOT NULL
		AND NOT EXISTS (	SELECT 1 FROM tracks
							WHERE tracks.album_id=albums.id
		)`)
	// delete artists without albums
	s.db.Exec(`
		DELETE FROM artists
		WHERE NOT EXISTS (	SELECT 1 from albums
							WHERE albums.tag_artist_id=artists.id
		)`)
	// finish up
	strNow := strconv.FormatInt(time.Now().Unix(), 10)
	s.db.SetSetting("last_scan_time", strNow)
	//
	log.Printf("finished clean in %s, -%d tracks\n",
		time.Since(start),
		deleted,
	)
	return nil
}

// items are passed to the handle*() functions
type item struct {
	fullPath  string
	relPath   string
	directory string
	filename  string
	stat      os.FileInfo
}

var coverFilenames = map[string]struct{}{
	"cover.png":   {},
	"cover.jpg":   {},
	"cover.jpeg":  {},
	"folder.png":  {},
	"folder.jpg":  {},
	"folder.jpeg": {},
	"album.png":   {},
	"album.jpg":   {},
	"album.jpeg":  {},
	"front.png":   {},
	"front.jpg":   {},
	"front.jpeg":  {},
}

// ## begin callbacks
// ## begin callbacks
// ## begin callbacks

func (s *Scanner) callbackItem(fullPath string, info *godirwalk.Dirent) error {
	stat, err := os.Stat(fullPath)
	if err != nil {
		return errors.Wrap(err, "stating")
	}
	relPath, err := filepath.Rel(s.musicPath, fullPath)
	if err != nil {
		return errors.Wrap(err, "getting relative path")
	}
	directory, filename := path.Split(relPath)
	it := &item{
		fullPath:  fullPath,
		relPath:   relPath,
		directory: directory,
		filename:  filename,
		stat:      stat,
	}
	isDir, err := info.IsDirOrSymlinkToDir()
	if err != nil {
		return errors.Wrap(err, "stating link to dir")
	}
	if isDir {
		return s.handleFolder(it)
	}
	lowerFilename := strings.ToLower(filename)
	if _, ok := coverFilenames[lowerFilename]; ok {
		s.curCover = filename
		return nil
	}
	ext := path.Ext(filename)
	if ext == "" {
		return nil
	}
	if _, ok := mime.Types[ext[1:]]; ok {
		return s.handleTrack(it)
	}
	return nil
}

func (s *Scanner) callbackPost(fullPath string, info *godirwalk.Dirent) error {
	defer func() {
		s.curCover = ""
	}()
	if s.trTxOpen {
		s.trTx.Commit()
		s.trTxOpen = false
	}
	// begin taking the current folder off the stack and add it's
	// parent, cover that we found, etc.
	folder := s.curFolders.Pop()
	if !folder.ReceivedPaths {
		return nil
	}
	folder.ParentID = s.curFolders.PeekID()
	folder.Cover = s.curCover
	s.db.Save(folder)
	// we only log changed folders
	log.Printf("processed folder `%s`\n",
		path.Join(folder.LeftPath, folder.RightPath))
	return nil
}

// decoded converts a string to it's latin equivalent. it will
// be used by the model's *UDec fields, and is only set if it
// differs from the original. the fields are used for searching
func decoded(in string) string {
	result := unidecode.Unidecode(in)
	if result == in {
		return ""
	}
	return result
}

// ## begin handlers
// ## begin handlers
// ## begin handlers

func (s *Scanner) handleFolder(it *item) error {
	if s.trTxOpen {
		// a transaction still being open when we handle a folder can
		// happen if there is a folder that contains /both/ tracks and
		// sub folders
		s.trTx.Commit()
		s.trTxOpen = false
	}
	folder := &db.Album{}
	defer func() {
		// folder's id will come from early return
		// or save at the end
		s.seenFolders[folder.ID] = struct{}{}
		s.curFolders.Push(folder)
	}()
	err := s.db.
		Select("id, updated_at").
		Where(db.Album{
			LeftPath:  it.directory,
			RightPath: it.filename,
		}).
		First(folder).
		Error
	if !gorm.IsRecordNotFoundError(err) &&
		it.stat.ModTime().Before(folder.UpdatedAt) {
		// we found the record but it hasn't changed
		return nil
	}
	folder.LeftPath = it.directory
	folder.RightPath = it.filename
	folder.RightPathUDec = decoded(it.filename)
	folder.ModifiedAt = it.stat.ModTime()
	s.db.Save(folder)
	folder.ReceivedPaths = true
	return nil
}

func (s *Scanner) handleTrack(it *item) error {
	if !s.trTxOpen {
		s.trTx = s.db.Begin()
		s.trTxOpen = true
	}
	// ** begin set track basics
	track := &db.Track{}
	err := s.trTx.
		Select("id, updated_at").
		Where(db.Track{
			AlbumID:  s.curFolders.PeekID(),
			Filename: it.filename,
		}).
		First(track).
		Error
	if !gorm.IsRecordNotFoundError(err) &&
		it.stat.ModTime().Before(track.UpdatedAt) {
		// we found the record but it hasn't changed
		s.seenTracks[track.ID] = struct{}{}
		return nil
	}
	track.Filename = it.filename
	track.FilenameUDec = decoded(it.filename)
	track.Size = int(it.stat.Size())
	track.AlbumID = s.curFolders.PeekID()
	trTags, err := tags.New(it.fullPath)
	if err != nil {
		// not returning the error here because we don't want
		// the entire walk to stop if we can't read the tags
		// of a single file
		log.Printf("error reading tags `%s`: %v", it.relPath, err)
		s.seenTracksErr++
		return nil
	}
	track.TagTitle = trTags.Title()
	track.TagTitleUDec = decoded(trTags.Title())
	track.TagTrackArtist = trTags.Artist()
	track.TagTrackNumber = trTags.TrackNumber()
	track.TagDiscNumber = trTags.DiscNumber()
	track.TagBrainzID = trTags.BrainzID()
	track.Length = trTags.Length()   // these two should be calculated
	track.Bitrate = trTags.Bitrate() // ...from the file instead of tags
	// ** begin set album artist basics
	artistName := func() string {
		if r := trTags.AlbumArtist(); r != "" {
			return r
		}
		if r := trTags.Artist(); r != "" {
			return r
		}
		return "Unknown Artist"
	}()
	artist := &db.Artist{}
	err = s.trTx.
		Select("id").
		Where("name=?", artistName).
		First(artist).
		Error
	if gorm.IsRecordNotFoundError(err) {
		artist.Name = artistName
		artist.NameUDec = decoded(artistName)
		s.trTx.Save(artist)
	}
	track.ArtistID = artist.ID
	// ** begin set genre
	genreName := func() string {
		if r := trTags.Genre(); r != "" {
			return r
		}
		return "Unknown Genre"
	}()
	genre := &db.Genre{}
	err = s.trTx.
		Select("id").
		Where("name=?", genreName).
		First(genre).
		Error
	if gorm.IsRecordNotFoundError(err) {
		genre.Name = genreName
		s.trTx.Save(genre)
	}
	track.TagGenreID = genre.ID
	// ** begin save the track
	s.trTx.Save(track)
	s.seenTracks[track.ID] = struct{}{}
	s.seenTracksNew++
	// ** begin set album if this is the first track in the folder
	folder := s.curFolders.Peek()
	if !folder.ReceivedPaths || folder.ReceivedTags {
		// the folder hasn't been modified or already has it's tags
		return nil
	}
	folder.TagTitle = trTags.Album()
	folder.TagTitleUDec = decoded(trTags.Album())
	folder.TagBrainzID = trTags.AlbumBrainzID()
	folder.TagYear = trTags.Year()
	folder.TagArtistID = artist.ID
	folder.TagGenreID = genre.ID
	folder.ReceivedTags = true
	return nil
}
