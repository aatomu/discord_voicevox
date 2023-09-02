package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"log"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/aatomu/atomicgo/discordbot"
	"github.com/aatomu/atomicgo/files"
	"github.com/aatomu/atomicgo/utils"
	"github.com/aatomu/slashlib"
	"github.com/bwmarrin/discordgo"
	"golang.org/x/text/language"
)

type Sessions struct {
	save   sync.Mutex
	guilds []*SessionData
}

type SessionData struct {
	guildID   string
	channelID string
	vcsession *discordgo.VoiceConnection
	lead      sync.Mutex
}

type UserSetting struct {
	Lang  string  `json:"language"`
	Speed float64 `json:"speed"`
	Pitch float64 `json:"pitch"`
	Type  int64   `json:"type"`
}

type VoicevoxSpeaker struct {
	SupportedFeatures struct {
		PermittedSynthesisMorphing string `json:"permitted_synthesis_morphing"`
	} `json:"supported_features"`
	Name        string `json:"name"`
	SpeakerUUID string `json:"speaker_uuid"`
	Styles      []struct {
		Name string `json:"name"`
		ID   int    `json:"id"`
	} `json:"styles"`
	Version string `json:"version"`
}

var (
	//変数定義
	clientID              = ""
	token                 = flag.String("token", "", "bot token")
	sessions              Sessions
	isVcSessionUpdateLock = false
	speakerList           = map[int]string{}
	dummy                 = UserSetting{
		Lang:  "auto",
		Speed: 1.5,
		Pitch: 1.1,
		Type:  1,
	}
	embedColor = 0x4169E1
)

func main() {
	//flag入手
	flag.Parse()
	fmt.Println("token        :", *token)

	//Voicevox 初期化
	VoicevoxInit()
	defer func() {
		fmt.Println("Stop Voicevox Docker")
		exec.Command("docker", "stop", "discord_voicevox")
	}()

	//discordBot 起動準備
	discord, err := discordbot.Init(*token)
	if err != nil {
		fmt.Println("Failed Bot Init", err)
		return
	}

	//eventトリガー設定
	discord.AddHandler(onReady)
	discord.AddHandler(onMessageCreate)
	discord.AddHandler(onInteractionCreate)
	discord.AddHandler(onVoiceStateUpdate)

	//起動
	discordbot.Start(discord)
	defer func() {
		fmt.Println("Stop DiscordBot")
		for _, session := range sessions.guilds {
			discord.ChannelMessageSendEmbed(session.channelID, &discordgo.MessageEmbed{
				Type:        "rich",
				Title:       "__Infomation__",
				Description: "Sorry. Bot will Shutdown. Will be try later.",
				Color:       embedColor,
			})
		}
		discord.Close()
	}()
	//起動メッセージ表示
	fmt.Println("Listening...")

	//bot停止対策
	<-utils.BreakSignal()
}

func VoicevoxInit() {
	//Voicevox 起動
	exec.Command("docker", "run", "--rm", "-itd", "-p", "127.0.0.1:50021:50021", "--name", "discord_voicevox", "voicevox/voicevox_engine:cpu-ubuntu20.04-latest").Run()
	//Voicevox Speaker入手
	res, err := http.Get("http://127.0.0.1:50021/speakers")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var speakers []VoicevoxSpeaker
	json.Unmarshal(body, &speakers)

	fmt.Println("Voicevox Speakers: ")
	for _, speaker := range speakers {
		for _, style := range speaker.Styles {
			name := speaker.Name + "-" + style.Name
			fmt.Printf("\"%s\":\"%d\",", name, style.ID)
			speakerList[style.ID] = name
		}
	}
	fmt.Println("")
}

// BOTの準備が終わったときにCall
func onReady(discord *discordgo.Session, r *discordgo.Ready) {
	clientID = discord.State.User.ID

	// コマンドの追加
	var minSpeed float64 = 0.5
	var minPitch float64 = 0.5
	new(slashlib.Command).
		//TTS
		AddCommand("join", "VoiceChatに接続します", discordgo.PermissionViewChannel).
		AddCommand("leave", "VoiceChatから切断します", discordgo.PermissionViewChannel).
		AddCommand("list", "Voicevoxのキャラクター名一覧を表示します", discordgo.PermissionViewChannel).
		AddCommand("get", "読み上げ設定を表示します", discordgo.PermissionViewChannel).
		AddCommand("set", "読み上げ設定を変更します", discordgo.PermissionViewChannel).
		AddOption(&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionNumber,
			Name:        "speed",
			Description: "読み上げ速度を設定",
			MinValue:    &minSpeed,
			MaxValue:    5,
		}).
		AddOption(&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionNumber,
			Name:        "pitch",
			Description: "声の高さを設定",
			MinValue:    &minPitch,
			MaxValue:    1.5,
		}).
		AddOption(&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionInteger,
			Name:        "type",
			Description: "読み上げキャラクターを設定",
		}).CommandCreate(discord, "")
}

// メッセージが送られたときにCall
func onMessageCreate(discord *discordgo.Session, m *discordgo.MessageCreate) {
	// state update
	joinedGuilds := len(discord.State.Guilds)
	joinedVC := len(sessions.guilds)
	VC := ""
	if joinedVC != 0 {
		VC = fmt.Sprintf(" %d鯖でお話し中", joinedVC)
	}
	discordbot.BotStateUpdate(discord, fmt.Sprintf("/join | %d鯖で稼働中 %s", joinedGuilds, VC), 0)

	mData := discordbot.MessageParse(discord, m)
	log.Println(mData.FormatText)

	// VCsession更新
	go func() {
		if isVcSessionUpdateLock {
			return
		}

		// 更新チェック
		isVcSessionUpdateLock = true
		defer func() {
			time.Sleep(1 * time.Minute)
			isVcSessionUpdateLock = false
		}()

		for i := range sessions.guilds {
			go func(n int) {
				session := sessions.guilds[n]
				session.lead.Lock()
				defer session.lead.Unlock()
				session.vcsession = discord.VoiceConnections[session.guildID]
			}(i)
		}
	}()

	// 読み上げ無し のチェック
	if strings.HasPrefix(m.Content, ";") {
		return
	}

	// debug
	if mData.UserID == "701336137012215818" {
		switch {
		case utils.RegMatch(mData.Message, "^!debug"):
			// セッション処理
			if utils.RegMatch(mData.Message, "[0-9]$") {
				guildID := utils.RegReplace(mData.Message, "", `^!debug\s*`)
				log.Println("Deleting SessionItem : " + guildID)
				sessions.Delete(guildID)
				return
			}

			// ユーザー一覧
			VCdata := map[string][]string{}
			for _, guild := range discord.State.Guilds {
				for _, vs := range guild.VoiceStates {
					user, err := discord.User(vs.UserID)
					if err != nil {
						continue
					}
					VCdata[vs.GuildID] = append(VCdata[vs.GuildID], user.String())
				}
			}

			// 表示
			for _, session := range sessions.guilds {
				guild, err := discord.Guild(session.guildID)
				if utils.PrintError("Failed Get GuildData by GuildID", err) {
					continue
				}

				channel, err := discord.Channel(session.channelID)
				if utils.PrintError("Failed Get ChannelData by ChannelID", err) {
					continue
				}

				embed, err := discord.ChannelMessageSendEmbed(mData.ChannelID, &discordgo.MessageEmbed{
					Type:        "rich",
					Title:       fmt.Sprintf("Guild:%s(%s)\nChannel:%s(%s)", guild.Name, session.guildID, channel.Name, session.channelID),
					Description: fmt.Sprintf("Members:```\n%s```", VCdata[guild.ID]),
					Color:       embedColor,
				})
				if err == nil {
					go func() {
						time.Sleep(30 * time.Second)
						err := discord.ChannelMessageDelete(mData.ChannelID, embed.ID)
						utils.PrintError("failed delete debug message", err)
					}()
				}
			}
			return
		case mData.Message == "?join":
			session := sessions.Get(mData.GuildID)

			if session.IsJoined() {
				return
			}

			JoinVoice(discord, m.GuildID, m.ChannelID, m.Author.ID, slashlib.InteractionResponse{})
			return
		}
	}

	//読み上げ
	session := sessions.Get(mData.GuildID)
	if session != nil {
		if session.IsJoined() && session.channelID == mData.ChannelID {
			session.Speech(mData.UserID, mData.Message)
			return
		}
	}
}

// InteractionCreate
func onInteractionCreate(discord *discordgo.Session, iData *discordgo.InteractionCreate) {
	// 表示&処理しやすく
	i := slashlib.InteractionViewAndEdit(discord, iData)

	// slashじゃない場合return
	if i.Check != slashlib.SlashCommand {
		return
	}

	// response用データ
	res := slashlib.InteractionResponse{
		Discord:     discord,
		Interaction: iData.Interaction,
	}

	session := sessions.Get(i.GuildID)
	// 分岐
	switch i.Command.Name {
	//TTS
	case "join":
		res.Thinking(false)

		if session.IsJoined() {
			Failed(res, "VoiceChat にすでに接続しています")
			return
		}

		JoinVoice(discord, i.GuildID, i.ChannelID, i.UserID, res)
		return

	case "leave":
		res.Thinking(false)

		if !session.IsJoined() {
			Failed(res, "VoiceChat に接続していません")
			return
		}

		session.Speech("BOT", "さいなら")
		Success(res, "グッバイ!")
		time.Sleep(1 * time.Second)
		session.vcsession.Disconnect()

		sessions.Delete(i.GuildID)
		return

	case "list":
		res.Thinking(false)

		var text string
		text = "```"
		n := 0
		for i := 0; true; i++ {
			name, ok := speakerList[i]
			if !ok {
				continue
			}
			text += fmt.Sprintf("%3d: %s\n", i, name)

			n++
			if n >= len(speakerList) {
				break
			}
		}
		text += "```"
		res.Follow(&discordgo.WebhookParams{
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:       "Voicevox Character List",
					Description: text,
				},
			},
		})
		return
	case "get":
		res.Thinking(false)

		result, err := userConfig(i.UserID, UserSetting{})
		if utils.PrintError("Failed Get Config", err) {
			Failed(res, "データのアクセスに失敗しました。")
			return
		}

		res.Follow(&discordgo.WebhookParams{
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:       fmt.Sprintf("@%s's Speech Config", i.UserName),
					Description: fmt.Sprintf("```\nLang  : %4s\nSpeed : %3.2f\nPitch : %3.2f\nType  : %4d(%s)```", result.Lang, result.Speed, result.Pitch, result.Type, speakerList[int(result.Type)]),
				},
			},
		})
		return

	case "set":
		res.Thinking(false)

		// 保存
		result, err := userConfig(i.UserID, UserSetting{})
		if utils.PrintError("Failed Get Config", err) {
			Failed(res, "読み上げ設定を読み込めませんでした")
			return
		}

		// チェック
		if newSpeed, ok := i.CommandOptions["speed"]; ok {
			result.Speed = newSpeed.FloatValue()
		}
		if newPitch, ok := i.CommandOptions["pitch"]; ok {
			result.Pitch = newPitch.FloatValue()
		}
		if newType, ok := i.CommandOptions["type"]; ok {
			result.Type = newType.IntValue()
		}
		if newLang, ok := i.CommandOptions["lang"]; ok {
			result.Lang = newLang.StringValue()
			// 言語チェック
			_, err := language.Parse(result.Lang)
			if result.Lang != "auto" && err != nil {
				Failed(res, "不明な言語です\n\"auto\"もしくは言語コードのみ使用可能です")
				return
			}
		}

		_, err = userConfig(i.UserID, result)
		if utils.PrintError("Failed Write Config", err) {
			Failed(res, "保存に失敗しました")
		}
		Success(res, "読み上げ設定を変更しました")
		return
	}
}

func userConfig(userID string, user UserSetting) (result UserSetting, err error) {
	//BOTチェック
	if userID == "BOT" {
		return UserSetting{
			Lang:  "ja",
			Speed: 1.75,
			Pitch: 1,
			Type:  1,
		}, nil
	}

	//ファイルパスの指定
	fileName := "./user_config.json"

	if !files.IsAccess(fileName) {
		if files.Create(fileName, false) != nil {
			return dummy, fmt.Errorf("failed Create Config File")
		}
	}

	bytes, err := os.ReadFile(fileName)
	if err != nil {
		return dummy, fmt.Errorf("failed Read Config File")
	}

	Users := map[string]UserSetting{}
	if string(bytes) != "" {
		err = json.Unmarshal(bytes, &Users)
		utils.PrintError("failed UnMarshal UserConfig", err)
	}

	// チェック用
	nilUserSetting := UserSetting{}
	//上書き もしくはデータ作成
	// result が  nil とき 書き込み
	if _, ok := Users[userID]; !ok {
		result = dummy
		if user == nilUserSetting {
			return
		}
	}
	if config, ok := Users[userID]; ok && user == nilUserSetting {
		return config, nil
	}

	// 書き込み
	if user != nilUserSetting {
		//lang
		if user.Lang != result.Lang {
			result.Lang = user.Lang
		}
		//speed
		if user.Speed != result.Speed {
			result.Speed = user.Speed
		}
		//pitch
		if user.Pitch != result.Pitch {
			result.Pitch = user.Pitch
		}
		//Type
		if user.Type != result.Type {
			result.Type = user.Type
		}
		//最後に書き込むテキストを追加(Write==trueの時)
		Users[userID] = result
		bytes, err = json.MarshalIndent(&Users, "", "  ")
		fmt.Println(string(bytes))
		if err != nil {
			return dummy, fmt.Errorf("failed Marshal UserConfig")
		}
		//書き込み
		files.WriteFileFlash(fileName, bytes)
		log.Println("User userConfig Writed")
	}
	return
}

// VCでJoin||Leaveが起きたときにCall
func onVoiceStateUpdate(discord *discordgo.Session, v *discordgo.VoiceStateUpdate) {
	vData := discordbot.VoiceStateParse(discord, v)
	if !vData.StatusUpdate.ChannelJoin {
		return
	}
	log.Println(vData.FormatText)

	//セッションがあるか確認
	session := sessions.Get(v.GuildID)
	if session == nil {
		return
	}
	vcChannelID := session.vcsession.ChannelID

	// ボイスチャンネルに誰かいるか
	isLeave := true
	for _, guild := range discord.State.Guilds {
		for _, vs := range guild.VoiceStates {
			if vcChannelID == vs.ChannelID && vs.UserID != clientID {
				isLeave = false
				break
			}
		}
	}

	if isLeave {
		// ボイスチャンネルに誰もいなかったら Disconnect する
		session.vcsession.Disconnect()
		sessions.Delete(v.GuildID)
	}
}

// Get Session
func (s *Sessions) Get(guildID string) *SessionData {
	for _, session := range s.guilds {
		if session.guildID != guildID {
			continue
		}
		return session
	}
	return nil
}

// Add Session
func (s *Sessions) Add(newSession *SessionData) {
	s.save.Lock()
	defer s.save.Unlock()
	s.guilds = append(s.guilds, newSession)
}

// Delete Session
func (s *Sessions) Delete(guildID string) {
	s.save.Lock()
	defer s.save.Unlock()
	var newSessions []*SessionData
	for _, session := range s.guilds {
		if session.guildID == guildID {
			if session.vcsession != nil {
				session.vcsession.Disconnect()
			}
			continue
		}
		newSessions = append(newSessions, session)
	}
	s.guilds = newSessions
}

// Join Voice
func JoinVoice(discord *discordgo.Session, guildID, channelID, userID string, res slashlib.InteractionResponse) {
	vcSession, err := discordbot.JoinUserVCchannel(discord, userID, false, true)
	if utils.PrintError("Failed Join VoiceChat", err) {
		if res.Discord != nil {
			Failed(res, "ユーザーが VoiceChatに接続していない\nもしくは権限が不足しています")
		}
		return
	}

	session := &SessionData{
		guildID:   guildID,
		channelID: channelID,
		vcsession: vcSession,
		lead:      sync.Mutex{},
	}

	sessions.Add(session)

	session.Speech("BOT", "おはー")
	if res.Discord != nil {
		Success(res, "ハロー!")
	}
}

// Is Joined Session
func (session *SessionData) IsJoined() bool {
	return session != nil
}

// Speech in Session
func (session *SessionData) Speech(userID string, text string) {
	// Special Character
	text = regexp.MustCompile(`<a?:[A-Za-z0-9_]+?:[0-9]+?>`).ReplaceAllString(text, "えもじ") // custom Emoji
	text = regexp.MustCompile(`<@[0-9]+?>`).ReplaceAllString(text, "あっと ゆーざー")             // user mention
	text = regexp.MustCompile(`<@&[0-9]+?>`).ReplaceAllString(text, "あっと ろーる")             // user mention
	text = regexp.MustCompile(`<#[0-9]+?>`).ReplaceAllString(text, "あっと ちゃんねる")            // channel
	text = regexp.MustCompile(`https?:.+`).ReplaceAllString(text, "ゆーあーるえる すーきっぷ")         // URL
	text = regexp.MustCompile(`(?s)\|\|.*\|\|`).ReplaceAllString(text, "ひみつ")              // hidden word
	// Word Decoration 3
	text = regexp.MustCompile(`>>> `).ReplaceAllString(text, "")                  // quote
	text = regexp.MustCompile("```.*```").ReplaceAllString(text, "こーどぶろっく すーきっぷ") // codeblock
	// Word Decoration 2
	text = regexp.MustCompile(`~~(.+)~~`).ReplaceAllString(text, "$1")     // strikethrough
	text = regexp.MustCompile(`__(.+)__`).ReplaceAllString(text, "$1")     // underlined
	text = regexp.MustCompile(`\*\*(.+)\*\*`).ReplaceAllString(text, "$1") // bold
	// Word Decoration 1
	text = regexp.MustCompile(`> `).ReplaceAllString(text, "")         // 1line quote
	text = regexp.MustCompile("`(.+)`").ReplaceAllString(text, "$1")   // code
	text = regexp.MustCompile(`_(.+)_`).ReplaceAllString(text, "$1")   // italic
	text = regexp.MustCompile(`\*(.+)\*`).ReplaceAllString(text, "$1") // bold
	// Delete black Newline
	text = regexp.MustCompile(`^\n+`).ReplaceAllString(text, "")
	// Delete More Newline
	if strings.Count(text, "\n") > 5 {
		str := strings.Split(text, "\n")
		text = strings.Join(str[:5], "\n")
		text += "以下略"
	}
	//text cut
	read := utils.StrCut(text, "以下略", 100)

	settingData, err := userConfig(userID, UserSetting{})
	utils.PrintError("Failed func userConfig()", err)

	if settingData.Lang == "auto" {
		settingData.Lang = "ja"
		if regexp.MustCompile(`^[a-zA-Z0-9\s.,]+$`).MatchString(text) {
			settingData.Lang = "en"
		}
	}

	//読み上げ待機
	session.lead.Lock()
	defer session.lead.Unlock()

	query, err := http.Post(fmt.Sprintf("http://127.0.0.1:50021/audio_query?speaker=%d&text=%s", settingData.Type, url.QueryEscape(read)), "application/json", nil)
	if err != nil {
		log.Println("Failed Get Voicevox Query", err)
		return
	}
	defer query.Body.Close()
	voice, err := http.Post(fmt.Sprintf("http://127.0.0.1:50021/synthesis?speaker=%d", settingData.Type), "application/json", query.Body)
	if err != nil {
		log.Println("Failed Get Voicevox Voice", err)
		return
	}
	defer voice.Body.Close()
	var end chan bool
	err = PlayAudioFile(settingData.Speed, settingData.Pitch, session.vcsession, voice.Body, false, end)
	utils.PrintError("Failed play Audio \""+read+"\" ", err)
}

// Command Failed Message
func Failed(res slashlib.InteractionResponse, description string) {
	_, err := res.Follow(&discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{
			{
				Title:       "Command Failed",
				Color:       embedColor,
				Description: description,
			},
		},
	})
	utils.PrintError("Failed send response", err)
}

// Command Success Message
func Success(res slashlib.InteractionResponse, description string) {
	_, err := res.Follow(&discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{
			{
				Title:       "Command Success",
				Color:       embedColor,
				Description: description,
			},
		},
	})
	utils.PrintError("Failed send response", err)
}

func PlayAudioFile(speed, pitch float64, vcsession *discordgo.VoiceConnection, r io.Reader, isPlayback bool, end <-chan bool) error {
	if err := vcsession.Speaking(true); err != nil {
		return err
	}
	defer vcsession.Speaking(false)

	done := make(chan error)
	stream := discordbot.NewMemEncodeStream(vcsession, r, discordbot.EncodeOpts{
		Compression: 1,
		AudioFilter: fmt.Sprintf("aresample=24000,asetrate=24000*%f/100,atempo=100/%f*%f", pitch*100, pitch*100, speed),
	}, done)

	ticker := time.NewTicker(time.Second)
	if !isPlayback {
		ticker.Stop()
	}

	for {
		select {
		case err := <-done:
			if err != nil && err != io.EOF {
				return err
			}
			stream.Cleanup()
			return nil
		case <-ticker.C:
			log.Printf("Sending Now... : Playback:%.2f(x%.2f)", stream.Status.Time.Seconds(), stream.Status.Speed)
		case <-end:
			stream.Cleanup()
			return nil
		}
	}
}
