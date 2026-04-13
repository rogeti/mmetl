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

func (t *Transformer) SlackParseUsers(data io.Reader) ([]SlackUser, error) {
	var users []SlackUser

	b, err := io.ReadAll(data)
	if err != nil {
		return users, err
	}

	t.Logger.Debugf("SlackParseUsers: Raw json input data: %s", string(b))

	err = json.Unmarshal(b, &users)
	if err != nil {
		t.Logger.Warnf("Slack Import: Error occurred when parsing some Slack users. Import may work anyway. err=%v", err)

		// This returns errors that are ignored
		return users, err
	}

	usersAsMaps := []map[string]any{}
	_ = json.Unmarshal(b, &usersAsMaps)

	for i, u := range users {
		t.Logger.Debugf("SlackParseUsers: Parsed user struct data %+v", u)
		t.Logger.Debugf("SlackParseUsers: Parsed user data as map %+v", usersAsMaps[i])
	}

	b, err = json.Marshal(users)
	if err != nil {
		return users, err
	}

	t.Logger.Debugf("SlackParseUsers: Marshalled users struct data: %s", string(b))

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
	var regexes = make(map[string]*regexp.Regexp, len(users))
	for _, user := range users {
		r, err := regexp.Compile("<@" + user.Id + `(\|` + user.Username + ")?>")
		if err != nil {
			t.Logger.Infof("Slack Import: Unable to compile the @mention, matching regular expression for the Slack user. username=%s user_id=%s", user.Username, user.Id)
			continue
		}
		regexes["@"+user.Username] = r
	}

	// Special cases.
	regexes["@here"], _ = regexp.Compile("<(!|@)here>")
	regexes["@channel"], _ = regexp.Compile("<!channel>")
	regexes["@all"], _ = regexp.Compile("<!everyone>")

	convertCount := 0
	for channelName, channelPosts := range posts {
		convertCount++
		t.Logger.Debugf("Slack Import: converting user mentions for channel %s. %v of %v", channelName, convertCount, len(posts))
		for postIdx, post := range channelPosts {
			for mention, r := range regexes {
				post.Text = r.ReplaceAllString(post.Text, mention)
				posts[channelName][postIdx] = post

				if post.Attachments != nil {
					for _, attachment := range post.Attachments {
						attachment.Fallback = r.ReplaceAllString(attachment.Fallback, mention)
					}
				}
			}
		}
	}

	t.Logger.Infof("Slack Import: Converted user mentions")
	return posts
}

func (t *Transformer) SlackConvertChannelMentions(channels []SlackChannel, posts map[string][]SlackPost) map[string][]SlackPost {
	var regexes = make(map[string]*regexp.Regexp, len(channels))
	for _, channel := range channels {
		r, err := regexp.Compile("<#" + channel.Id + `(\|` + channel.Name + ")?>")
		if err != nil {
			t.Logger.Infof("Slack Import: Unable to compile the !channel, matching regular expression for the Slack channel. channel_id=%s channel_name=%s", channel.Id, channel.Name)
			continue
		}
		regexes["~"+channel.Name] = r
	}

	convertCount := 0
	for channelName, channelPosts := range posts {
		convertCount++
		t.Logger.Debugf("Slack Import: converting channel mentions for channel %s. %v of %v", channelName, convertCount, len(posts))
		for postIdx, post := range channelPosts {
			for channelReplace, r := range regexes {
				post.Text = r.ReplaceAllString(post.Text, channelReplace)
				posts[channelName][postIdx] = post

				if post.Attachments != nil {
					for _, attachment := range post.Attachments {
						attachment.Fallback = r.ReplaceAllString(attachment.Fallback, channelReplace)
					}
				}
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
				slackExport.Channels = append(slackExport.Channels, slackExport.PublicChannels...)
			case "dms.json":
				slackExport.DirectChannels, _ = t.SlackParseChannels(reader, model.ChannelTypeDirect)
				slackExport.Channels = append(slackExport.Channels, slackExport.DirectChannels...)
			case "groups.json":
				slackExport.PrivateChannels, _ = t.SlackParseChannels(reader, model.ChannelTypePrivate)
				slackExport.Channels = append(slackExport.Channels, slackExport.PrivateChannels...)
			case "mpims.json":
				slackExport.GroupChannels, _ = t.SlackParseChannels(reader, model.ChannelTypeGroup)
				slackExport.Channels = append(slackExport.Channels, slackExport.GroupChannels...)
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
		slackExport.Posts = t.SlackConvertChannelMentions(slackExport.Channels, slackExport.Posts)
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
