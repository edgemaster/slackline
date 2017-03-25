package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/nlopes/slack"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

type Team struct {
	Id string
	*slack.Client
	IncomingToken string
}

func NewTeam(s string) *Team {
	parts := strings.Split(s, ":")
	return &Team{parts[0], slack.New(parts[1]), parts[2]}
}

type Channel struct {
	TeamId    string
	ChannelId string `json:"channel"`
}

func MakeChannel(s string) Channel {
	parts := strings.Split(s, "/")
	return Channel{parts[0], parts[1]}
}

func (c *Channel) GetTeam() *Team {
	return config.teams[c.TeamId]
}

func (c Channel) Forward(f func(Channel)) {
	for _, other := range config.channelMap[c] {
		if c != other {
			f(other)
		}
	}
}

type Configuration struct {
	teams          map[string]*Team
	channelMap     map[Channel][]Channel
	outboundTokens map[Channel]string
}

// Configuration format:
// SLACKLINE_TEAMS=TEAM_ID:API_TOKEN:INCOMING_TOKEN,...
// Incoming tokens are of the format Bxxxxxxx/xxxxxxxxxxxxxxx
//
// SLACKLINE_CHANNEL_MAP=TID/CID:TID/CID:TID/CID,...
// SLACKLINE_OUTBOUND_TOKENS=TID/CID:OUTGOING_TOKEN,...
func GetConfiguration() *Configuration {
	team_strs := strings.Split(os.Getenv("SLACKLINE_TEAMS"), ",")
	teams := make(map[string]*Team, len(team_strs))

	for _, team_str := range team_strs {
		team := NewTeam(team_str)
		teams[team.Id] = team
	}

	channels_strs := strings.Split(os.Getenv("SLACKLINE_CHANNEL_MAP"), ",")
	channelMap := make(map[Channel][]Channel, len(channels_strs)*3)
	for _, channels_str := range channels_strs {
		channel_strs := strings.Split(channels_str, ":")
		channels := make([]Channel, len(channel_strs))

		for key, channel_str := range channel_strs {
			channel := MakeChannel(channel_str)
			channels[key] = channel

			if _, present := channelMap[channel]; !present {
				channelMap[channel] = channels
			} else {
				panic(fmt.Sprintf("%s already present in channel map configuration.", channel_str))
			}
		}
	}

	tokens := strings.Split(os.Getenv("SLACKLINE_OUTBOUND_TOKENS"), ",")
	outboundTokens := make(map[Channel]string, len(tokens))
	for _, token := range tokens {
		parts := strings.Split(token, ":")
		outboundTokens[MakeChannel(parts[0])] = parts[1]
	}

	return &Configuration{teams, channelMap, outboundTokens}
}

var config *Configuration

func (c Channel) VerifyToken(token string) bool {
	return config.outboundTokens[c] == token
}

type slackMessage struct {
	Channel
	Username  string `json:"username"`
	Text      string `json:"text"`
	Icon      string `json:"icon_url"`
	LinkNames bool   `json:"link_names"`
}

func (s *slackMessage) payload() io.Reader {
	s.LinkNames = true
	content, _ := json.Marshal(s)
	return bytes.NewReader(content)
}

var mentionRegexp = regexp.MustCompile("<@[^>]+>")

func (msg *slackMessage) RewriteMentions() {
	text := mentionRegexp.ReplaceAllStringFunc(msg.Text, func(s string) string {
		s = s[2 : len(s)-1]
		if strings.Contains(s, "|") {
			s = strings.Split(s, "|")[1]
		} else {
			user, err := msg.GetTeam().GetUserInfo(s)
			if err == nil {
				log.Printf("Unable to map %v to username: %v", s, err)
			} else {
				s = user.Name
			}
		}
		return "@" + s
	})
	msg.Text = text
}

func (msg *slackMessage) FetchUserIcon() error {
	userInfo, err := msg.GetTeam().GetUserInfo(msg.Username)
	if err == nil {
		msg.Icon = userInfo.Profile.ImageOriginal
	}
	return err
}

func (c Channel) WebhookPostMessage(msg *slackMessage) (err error) {

	const postMessageURL = "https://hooks.slack.com/services/"
	team := c.GetTeam()

	res, err := http.Post(
		postMessageURL+"/"+team.Id+"/"+team.IncomingToken,
		"application/json",
		msg.payload(),
	)

	if err != nil {
		log.Println(err)
		return err
	}

	if res.StatusCode != 200 {
		defer res.Body.Close()
		body, _ := ioutil.ReadAll(res.Body)
		err := errors.New(res.Status + " - " + string(body))
		log.Println(err)
		return err
	}

	return
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("$PORT must be set")
	}

	config = GetConfiguration()

	router := gin.Default()

	router.POST("/bridge", func(c *gin.Context) {
		msg := slackMessage{
			Channel:  Channel{c.PostForm("team_id"), c.PostForm("channel_id")},
			Username: c.PostForm("user_name"),
			Text:     c.PostForm("text"),
		}

		c.Status(200)

		if !msg.VerifyToken(c.PostForm("token")) {
			log.Printf("Incorrect webhook token: %v", c.PostForm("token"))
			return
		}

		if msg.Username == "slackbot" {
			return
		}

		msg.FetchUserIcon()
		msg.RewriteMentions()

		msg.Forward(func(c Channel) {
			c.WebhookPostMessage(&msg)
		})
	})
	router.Run(":" + port)
}
