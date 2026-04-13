package slack

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

var (
	// Slack user IDs start with U (or W for enterprise Grid), channel IDs with C or G
	// (private channels and group DMs), followed by alphanumeric characters
	// (e.g., U0A1B2C3D, W0A1B2C3D, C04MXABCD, G024BE91L).
	slackUserMentionRe    = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	slackChannelMentionRe = regexp.MustCompile(`<#([CG][A-Z0-9]+)(?:\|[^>]*)?>`)
	// Matches special broadcast mentions in both bare and pipe-aliased forms, e.g. <!here>, <!here|here>, <@here>.
	slackSpecialMentionRe = regexp.MustCompile(`<!(here|channel|everyone)(?:\|[^>]*)?>|<@here>`)
)

// replaceMentions replaces Slack mention patterns in text using a single regex
// and a lookup map, instead of compiling one regex per entity.
func replaceMentions(text string, re *regexp.Regexp, prefixLen int, lookup map[string]string) string {
	return re.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[prefixLen : len(match)-1] // strip prefix (<@ or <#) and closing >
		id := inner
		if pipeIdx := strings.IndexByte(inner, '|'); pipeIdx >= 0 {
			id = inner[:pipeIdx]
		}
		if replacement, ok := lookup[id]; ok {
			return replacement
		}
		return match
	})
}

func replaceUserMentionsInText(text string, lookup map[string]string) string {
	text = slackSpecialMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		switch {
		case strings.Contains(match, "here"):
			return "@here"
		case strings.Contains(match, "channel"):
			return "@channel"
		case strings.Contains(match, "everyone"):
			return "@all"
		}
		return match
	})
	return replaceMentions(text, slackUserMentionRe, 2, lookup)
}

func (t *Transformer) SlackParseUsers(data io.Reader) ([]SlackUser, error) {
	decoder := json.NewDecoder(data)

	var users []SlackUser
	if err := decoder.Decode(&users); err != nil {
		t.Logger.Warnf("Slack Import: Error occurred when parsing some Slack users. Import may work anyway. err=%v", err)
		return users, err
	}

	for _, u := range users {
		t.Logger.Debugf("SlackParseUsers: Parsed user struct data %+v", u)
	}

	return users, nil
}

func (t *Transformer) SlackParseChannels(data io.Reader, channelType model.ChannelType) ([]SlackChannel, error) {
	decoder := json.NewDecoder(data)

	var channels []SlackChannel
	if err := decoder.Decode(&channels); err != nil {
		t.Logger.Warnf("Slack Import: Error occurred when parsing some Slack channels. Import may work anyway. err=%v", err)
		return channels, err
	}

	for i := range channels {
		channels[i].Type = channelType
	}

	return channels, nil
}

func (t *Transformer) SlackParsePosts(data io.Reader) ([]SlackPost, error) {
	decoder := json.NewDecoder(data)

	var posts []SlackPost
	if err := decoder.Decode(&posts); err != nil {
		t.Logger.Warnf("Slack Import: Error occurred when parsing some Slack posts. Import may work anyway. err=%v", err)
		return posts, err
	}
	return posts, nil
}

func (t *Transformer) SlackConvertUserMentions(users []SlackUser, posts map[string][]SlackPost) map[string][]SlackPost {
	userLookup := make(map[string]string, len(users))
	for _, user := range users {
		userLookup[user.Id] = "@" + user.Username
	}

	convertCount := 0
	for channelName, channelPosts := range posts {
		convertCount++
		t.Logger.Debugf("Slack Import: converting user mentions for channel %s. %v of %v", channelName, convertCount, len(posts))
		for postIdx := range channelPosts {
			channelPosts[postIdx].Text = replaceUserMentionsInText(channelPosts[postIdx].Text, userLookup)

			for _, attachment := range channelPosts[postIdx].Attachments {
				attachment.Fallback = replaceUserMentionsInText(attachment.Fallback, userLookup)
			}
		}
	}

	t.Logger.Infof("Slack Import: Converted user mentions")
	return posts
}

func (t *Transformer) SlackConvertChannelMentions(channels []SlackChannel, posts map[string][]SlackPost) map[string][]SlackPost {
	channelLookup := make(map[string]string, len(channels))
	for _, channel := range channels {
		channelLookup[channel.Id] = "~" + channel.Name
	}

	convertCount := 0
	for channelName, channelPosts := range posts {
		convertCount++
		t.Logger.Debugf("Slack Import: converting channel mentions for channel %s. %v of %v", channelName, convertCount, len(posts))
		for postIdx := range channelPosts {
			channelPosts[postIdx].Text = replaceMentions(channelPosts[postIdx].Text, slackChannelMentionRe, 2, channelLookup)

			for _, attachment := range channelPosts[postIdx].Attachments {
				attachment.Fallback = replaceMentions(attachment.Fallback, slackChannelMentionRe, 2, channelLookup)
			}
		}
	}

	t.Logger.Infof("Slack Import: Converted channel mentions")

	return posts
}

func (t *Transformer) SlackConvertPostsMarkup(posts map[string][]SlackPost) map[string][]SlackPost {
	regexReplaceAllString := []struct {
		regex *regexp.Regexp
		rpl   string
	}{
		// URL
		{
			regexp.MustCompile(`<([^|<>]+)\|([^|<>]+)>`),
			"[$2]($1)",
		},
		// bold
		{
			regexp.MustCompile(`(^|[\s.;,])\*(\S[^*\n]+)\*`),
			"$1**$2**",
		},
		// strikethrough
		{
			regexp.MustCompile(`(^|[\s.;,])\~(\S[^~\n]+)\~`),
			"$1~~$2~~",
		},
		// single paragraph blockquote
		// Slack converts > character to &gt;
		{
			regexp.MustCompile(`(?sm)^&gt;`),
			">",
		},
	}

	regexReplaceAllStringFunc := []struct {
		regex *regexp.Regexp
		fn    func(string) string
	}{
		// multiple paragraphs blockquotes
		{
			regexp.MustCompile(`(?sm)^>&gt;&gt;(.+)$`),
			func(src string) string {
				// remove >>> prefix, might have leading \n
				prefixRegexp := regexp.MustCompile(`^([\n])?>&gt;&gt;(.*)`)
				src = prefixRegexp.ReplaceAllString(src, "$1$2")
				// append > to start of line
				appendRegexp := regexp.MustCompile(`(?m)^`)
				return appendRegexp.ReplaceAllString(src, ">$0")
			},
		},
	}

	convertCount := 0
	for channelName, channelPosts := range posts {
		convertCount++
		t.Logger.Debugf("Slack Import: converting markdown for channel %s. %v of %v", channelName, convertCount, len(posts))

		for postIdx, post := range channelPosts {
			result := post.Text

			for _, rule := range regexReplaceAllString {
				result = rule.regex.ReplaceAllString(result, rule.rpl)
			}

			for _, rule := range regexReplaceAllStringFunc {
				result = rule.regex.ReplaceAllStringFunc(result, rule.fn)
			}
			// Don't truncate here - splitting will happen later in the transformation phase
			posts[channelName][postIdx].Text = result
		}
	}

	t.Logger.Infof("Slack Import: Converted markdown")

	return posts
}

func (t *Transformer) ParseSlackExportFile(zipReader *zip.Reader, skipConvertPosts bool) (*SlackExport, error) {
	slackExport := SlackExport{TeamName: t.TeamName}
	slackExport.Posts = make(map[string][]SlackPost)
	slackExport.Uploads = make(map[string]*zip.File)
	numFiles := len(zipReader.File)

	for i, file := range zipReader.File {
		err := func(i int, file *zip.File) error {
			t.Logger.Infof("Processing file %d of %d: %s", i+1, numFiles, file.Name)

			reader, err := file.Open()
			if err != nil {
				return err
			}
			defer reader.Close()

			switch file.Name {
			case "channels.json":
				slackExport.PublicChannels, _ = t.SlackParseChannels(reader, model.ChannelTypeOpen)
			case "dms.json":
				slackExport.DirectChannels, _ = t.SlackParseChannels(reader, model.ChannelTypeDirect)
			case "groups.json":
				slackExport.PrivateChannels, _ = t.SlackParseChannels(reader, model.ChannelTypePrivate)
			case "mpims.json":
				slackExport.GroupChannels, _ = t.SlackParseChannels(reader, model.ChannelTypeGroup)
			case "users.json":
				usersJSONFileName := os.Getenv("USERS_JSON_FILE")
				if usersJSONFileName != "" {
					reader.Close()
					reader, err = os.Open(usersJSONFileName)
					if err != nil {
						return errors.Wrap(err, "failed to read users file from USERS_JSON_FILE")
					}
					defer reader.Close()
				}

				users, _ := t.SlackParseUsers(reader)
				slackExport.Users = users
			default:
				spl := strings.Split(file.Name, "/")
				if len(spl) == 2 && strings.HasSuffix(spl[1], ".json") {
					newposts, _ := t.SlackParsePosts(reader)
					channel := spl[0]
					if _, ok := slackExport.Posts[channel]; !ok {
						slackExport.Posts[channel] = newposts
					} else {
						slackExport.Posts[channel] = append(slackExport.Posts[channel], newposts...)
					}
				} else if len(spl) == 3 && spl[0] == "__uploads" {
					slackExport.Uploads[spl[1]] = file
				}
			}

			return nil
		}(i, file)

		if err != nil {
			return nil, err
		}
	}

	if !skipConvertPosts {
		t.Logger.Info("Converting post mentions and markup")
		start := time.Now()
		slackExport.Posts = t.SlackConvertUserMentions(slackExport.Users, slackExport.Posts)
		slackExport.Posts = t.SlackConvertChannelMentions(slackExport.AllChannels(), slackExport.Posts)
		slackExport.Posts = t.SlackConvertPostsMarkup(slackExport.Posts)
		elapsed := time.Since(start)
		t.Logger.Debugf("Converting mentions finished (%s)", elapsed)
	}

	return &slackExport, nil
}

// ParseSlackExportMetadata parses only the metadata files (users, channels) without loading posts
func (t *Transformer) ParseSlackExportMetadata(zipReader *zip.Reader) (*SlackExport, error) {
	t.Logger.Info("Parsing metadata from Slack export")
	slackExport := SlackExport{TeamName: t.TeamName}

	for _, file := range zipReader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		switch file.Name {
		case "channels.json":
			reader, err := file.Open()
			if err != nil {
				return nil, err
			}
			channels, _ := t.SlackParseChannels(reader, model.ChannelTypeOpen)
			slackExport.PublicChannels = channels
			slackExport.Channels = append(slackExport.Channels, channels...)
			reader.Close()

		case "dms.json":
			reader, err := file.Open()
			if err != nil {
				return nil, err
			}
			channels, _ := t.SlackParseChannels(reader, model.ChannelTypeDirect)
			slackExport.DirectChannels = channels
			slackExport.Channels = append(slackExport.Channels, channels...)
			reader.Close()

		case "groups.json":
			reader, err := file.Open()
			if err != nil {
				return nil, err
			}
			channels, _ := t.SlackParseChannels(reader, model.ChannelTypePrivate)
			slackExport.PrivateChannels = channels
			slackExport.Channels = append(slackExport.Channels, channels...)
			reader.Close()

		case "mpims.json":
			reader, err := file.Open()
			if err != nil {
				return nil, err
			}
			channels, _ := t.SlackParseChannels(reader, model.ChannelTypeGroup)
			slackExport.GroupChannels = channels
			slackExport.Channels = append(slackExport.Channels, channels...)
			reader.Close()

		case "users.json":
			usersJSONFileName := os.Getenv("USERS_JSON_FILE")
			var reader io.ReadCloser
			var err error

			if usersJSONFileName != "" {
				reader, err = os.Open(usersJSONFileName)
				if err != nil {
					return nil, errors.Wrap(err, "failed to read users file from USERS_JSON_FILE")
				}
			} else {
				reader, err = file.Open()
				if err != nil {
					return nil, err
				}
			}

			users, _ := t.SlackParseUsers(reader)
			slackExport.Users = users
			reader.Close()
		}
	}

	return &slackExport, nil
}

// GetChannelDirectories returns a list of all channel directories in the zip
func (t *Transformer) GetChannelDirectories(zipReader *zip.Reader) []string {
	channelDirs := make(map[string]bool)

	for _, file := range zipReader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		// Check for channel post files (e.g., "general/2024-01-01.json")
		parts := strings.Split(file.Name, "/")
		if len(parts) == 2 && strings.HasSuffix(parts[1], ".json") && parts[0] != "__uploads" {
			channelDirs[parts[0]] = true
		}
	}

	// Convert map to sorted slice for deterministic processing
	result := make([]string, 0, len(channelDirs))
	for dir := range channelDirs {
		result = append(result, dir)
	}

	return result
}

// ParseChannelPosts parses posts for a single channel directory
func (t *Transformer) ParseChannelPosts(zipReader *zip.Reader, channelDir string) ([]SlackPost, map[string]*zip.File, error) {
	var posts []SlackPost
	uploads := make(map[string]*zip.File)

	for _, file := range zipReader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		parts := strings.Split(file.Name, "/")

		// Check if this file belongs to the current channel
		if len(parts) == 2 && parts[0] == channelDir && strings.HasSuffix(parts[1], ".json") {
			reader, err := file.Open()
			if err != nil {
				return nil, nil, err
			}

			newPosts, _ := t.SlackParsePosts(reader)
			posts = append(posts, newPosts...)
			reader.Close()
		}

		// Collect uploads for this channel
		if len(parts) == 3 && parts[0] == "__uploads" {
			uploads[parts[1]] = file
		}
	}

	return posts, uploads, nil
}

// ParseSlackExportMetadataFromDir reads metadata (users, channels) from a filesystem directory.
func (t *Transformer) ParseSlackExportMetadataFromDir(dirPath string) (*SlackExport, error) {
	t.Logger.Info("Parsing metadata from Slack export directory")
	slackExport := SlackExport{TeamName: t.TeamName}

	type channelFile struct {
		name     string
		chanType model.ChannelType
		target   *[]SlackChannel
	}

	channelFiles := []channelFile{
		{"channels.json", model.ChannelTypeOpen, &slackExport.PublicChannels},
		{"dms.json", model.ChannelTypeDirect, &slackExport.DirectChannels},
		{"groups.json", model.ChannelTypePrivate, &slackExport.PrivateChannels},
		{"mpims.json", model.ChannelTypeGroup, &slackExport.GroupChannels},
	}

	for _, cf := range channelFiles {
		filePath := dirPath + "/" + cf.name
		reader, err := os.Open(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		channels, _ := t.SlackParseChannels(reader, cf.chanType)
		*cf.target = channels
		slackExport.Channels = append(slackExport.Channels, channels...)
		reader.Close()
	}

	// Parse users
	usersPath := dirPath + "/users.json"
	usersReader, err := os.Open(usersPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read users.json")
	}
	users, _ := t.SlackParseUsers(usersReader)
	slackExport.Users = users
	usersReader.Close()

	return &slackExport, nil
}

// GetChannelDirectoriesFromDir returns a list of all channel directories in a filesystem directory.
func (t *Transformer) GetChannelDirectoriesFromDir(dirPath string) []string {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		t.Logger.Warnf("Failed to read directory %s: %v", dirPath, err)
		return nil
	}

	var result []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip metadata files and internal directories
		name := entry.Name()
		if name == "__uploads" || strings.HasPrefix(name, ".") {
			continue
		}

		// Check if this directory contains any JSON files (channel posts)
		subEntries, err := os.ReadDir(dirPath + "/" + name)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if !sub.IsDir() && strings.HasSuffix(sub.Name(), ".json") {
				result = append(result, name)
				break
			}
		}
	}

	return result
}

// ParseChannelPostsFromDir parses posts for a single channel from a filesystem directory.
// Returns posts and a map of FileId -> disk path for any files that exist on disk.
func (t *Transformer) ParseChannelPostsFromDir(dirPath, channelDir string) ([]SlackPost, map[string]string, error) {
	var posts []SlackPost
	diskFiles := make(map[string]string)

	channelPath := dirPath + "/" + channelDir
	entries, err := os.ReadDir(channelPath)
	if err != nil {
		return nil, nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		filePath := channelPath + "/" + entry.Name()
		reader, err := os.Open(filePath)
		if err != nil {
			t.Logger.Warnf("Failed to open %s: %v", filePath, err)
			continue
		}

		newPosts, _ := t.SlackParsePosts(reader)
		posts = append(posts, newPosts...)
		reader.Close()
	}

	return posts, diskFiles, nil
}
