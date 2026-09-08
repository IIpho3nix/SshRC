package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/wish/v2"
	bm "charm.land/wish/v2/bubbletea"
	lm "charm.land/wish/v2/logging"
	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type Config struct {
	ServerName     string
	Port           int
	ServerPassword string
	ChatlogPath    string
	UserDBPath     string
	IdentityPath   string
	Paranoid       bool
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

const MaxMessageRunes = 1024

func sanitizeText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = ansiRegex.ReplaceAllString(s, "")

	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r >= 32 && r != 127:
			b.WriteRune(r)
		}
	}

	out := b.String()

	r := []rune(out)
	if len(r) > MaxMessageRunes {
		out = string(r[:MaxMessageRunes])
	}

	return out
}

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{1,20}$`)

func validateNick(n string) (string, bool) {
	n = sanitizeText(n)
	n = strings.ReplaceAll(n, " ", "_")

	if strings.EqualFold(n, "system") {
		return "", false
	}

	if !usernameRegex.MatchString(n) {
		return "", false
	}

	return n, true
}

var hexColorRegex = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)

func validateColor(input string) (string, bool) {
	input = strings.TrimSpace(input)

	if hexColorRegex.MatchString(input) {
		return strings.ToUpper(input), true
	}

	if n, err := strconv.Atoi(input); err == nil {
		if n >= 0 && n <= 255 {
			return input, true
		}
	}

	return "", false
}

type UserProfile struct {
	Nick  string `json:"nick"`
	Color string `json:"color"`
}

type UserStore struct {
	mu    sync.Mutex
	Users map[string]UserProfile `json:"users"`
}

func NewUserStore() *UserStore {
	return &UserStore{
		Users: make(map[string]UserProfile),
	}
}

func (u *UserStore) Load(path string) error {
	if config.Paranoid {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(&u.Users)
}

func (u *UserStore) Save(path string) error {
	if config.Paranoid {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(u.Users); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func (u *UserStore) Get(login string) (UserProfile, bool) {
	if config.Paranoid {
		return UserProfile{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	file, ok := u.Users[login]
	return file, ok
}

func (u *UserStore) Set(login string, p UserProfile) {
	if config.Paranoid {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	u.Users[login] = p
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

func getTempFilePath(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	filename := prefix + hex.EncodeToString(bytes)
	if !directoryExists(filepath.Join(os.TempDir(), "ssh_tmp")) {
		if err := os.MkdirAll(filepath.Join(os.TempDir(), "ssh_tmp"), 0755); err != nil {
			return "", err
		}
	}

	return filepath.Join(filepath.Join(os.TempDir(), "ssh_tmp"), filename), nil
}

var config Config

func loadConfig() {
	configFile := flag.String("config", "", "Path to config file (e.g., config.env)")
	flag.StringVar(&config.ServerName, "server_name", "SshRC", "Name of the server")
	flag.IntVar(&config.Port, "port", 2222, "Port to listen on")
	flag.StringVar(&config.ServerPassword, "server_password", "", "Server password (optional)")
	flag.StringVar(&config.ChatlogPath, "chatlog_path", ".data/chat.jsonl", "Path to save chat history")
	flag.StringVar(&config.UserDBPath, "user_db_path", ".data/users.json", "Path to user database file")
	flag.StringVar(&config.IdentityPath, "identity_path", ".data/SshRC_ed25519", "Path to SSH identity file")
	flag.BoolVar(&config.Paranoid, "paranoid", false, "Enable paranoid mode (No logs, no history, random identity)")
	flag.Parse()

	if *configFile != "" {
		file, err := os.Open(*configFile)
		if err != nil {
			log.Fatalf("Failed to open config file: %v", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
				switch key {
				case "server_name":
					config.ServerName = val
				case "port":
					fmt.Sscanf(val, "%d", &config.Port)
				case "server_password":
					config.ServerPassword = val
				case "chatlog_path":
					config.ChatlogPath = val
				case "user_db_path":
					config.UserDBPath = val
				case "identity_path":
					config.IdentityPath = val
				case "paranoid":
					config.Paranoid, _ = strconv.ParseBool(val)
				}
			}
		}
	}

	if config.Paranoid {
		config.IdentityPath, _ = getTempFilePath("")
	}
}

type ChatMessage struct {
	Timestamp    time.Time `json:"timestamp"`
	Username     string    `json:"username"`
	Color        string    `json:"color"`
	Text         string    `json:"text"`
	IsAction     bool      `json:"is_action"`
	Continuation bool      `json:"-"`
}

type Room struct {
	mu      sync.Mutex
	clients map[chan ChatMessage]*ClientInfo
	history []ChatMessage
	logFile *os.File
}

type ClientInfo struct {
	Username string
	IP       string
}

func NewRoom() *Room {
	r := &Room{
		clients: make(map[chan ChatMessage]*ClientInfo),
		history: make([]ChatMessage, 0),
	}
	r.loadHistory()
	return r
}

func (r *Room) loadHistory() {
	if config.Paranoid {
		return
	}
	file, err := os.OpenFile(config.ChatlogPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("Failed to open chatlog: %v", err)
		return
	}
	r.logFile = file

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var msg ChatMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err == nil {
			r.history = append(r.history, msg)
		}
	}

	if len(r.history) > 500 {
		r.history = r.history[len(r.history)-500:]
	}
}

func (r *Room) Subscribe(username string, ip string) chan ChatMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := make(chan ChatMessage, 500)
	r.clients[c] = &ClientInfo{Username: username, IP: ip}
	return c
}

func (r *Room) Unsubscribe(c chan ChatMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, c)
	close(c)
}

func (r *Room) UpdateUsername(c chan ChatMessage, newName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c]; ok {
		r.clients[c].Username = newName
	}
}

func (r *Room) rewriteLogFile() {
	if r.logFile != nil {
		r.logFile.Close()
	}

	tmpPath := config.ChatlogPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("Failed to create temp log: %v", err)
		return
	}

	for _, msg := range r.history {
		data, _ := json.Marshal(msg)
		f.Write(append(data, '\n'))
	}
	f.Close()

	os.Rename(tmpPath, config.ChatlogPath)

	r.logFile, err = os.OpenFile(config.ChatlogPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("Failed to re-open log: %v", err)
	}
}

func (r *Room) Broadcast(msg ChatMessage) {
	msg.Text = sanitizeText(msg.Text)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !config.Paranoid {
		r.history = append(r.history, msg)

		if len(r.history) > 500 {
			r.history = r.history[250:]
			r.rewriteLogFile()
		} else if r.logFile != nil {
			data, _ := json.Marshal(msg)
			r.logFile.Write(append(data, '\n'))
		}
	}

	for c := range r.clients {
		select {
		case c <- msg:
		default:
		}
	}
}

func (r *Room) PrivateMessage(from, to string, msg ChatMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c, ci := range r.clients {
		if ci.Username == from || ci.Username == to {
			select {
			case c <- msg:
			default:
			}
		}
	}
}

var (
	globalRoom *Room
	userStore  *UserStore
)

var (
	timeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	messageStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	systemStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Italic(true)
	borderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

func formatMessage(m ChatMessage, availableWidth int) string {
	if m.Continuation {
		return messageStyle.Render(m.Text)
	}

	timeStr := timeStyle.Render(fmt.Sprintf("[%s]", m.Timestamp.Format("15:04")))
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.Color))

	var prefix string
	if m.IsAction {
		prefix = fmt.Sprintf("%s %s ", timeStr, nameStyle.Render("* "+m.Username))
	} else if m.Username == "SYSTEM" {
		prefix = fmt.Sprintf("%s %s: ", timeStr, nameStyle.Render("<SYSTEM>"))
	} else {
		prefix = fmt.Sprintf("%s %s: ", timeStr, nameStyle.Render("<"+m.Username+">"))
	}

	prefixWidth := lipgloss.Width(prefix)
	maxTextWidth := availableWidth - prefixWidth

	textStyle := lipgloss.NewStyle().Width(maxTextWidth)

	var renderedText string
	if m.IsAction || m.Username == "SYSTEM" {
		renderedText = systemStyle.Render(m.Text)
	} else {
		renderedText = messageStyle.Render(m.Text)
	}

	wrappedText := textStyle.Render(renderedText)

	return prefix + wrappedText
}

type model struct {
	login        string
	username     string
	userColor    string
	width        int
	height       int
	input        textinput.Model
	sub          chan ChatMessage
	messages     []ChatMessage
	quitChan     chan struct{}
	scrollOffset int
}

func waitForMessage(sub chan ChatMessage) tea.Cmd {
	return func() tea.Msg { return <-sub }
}

func (m model) Init() tea.Cmd {
	return tea.Batch(cursor.Blink, waitForMessage(m.sub))
}

func (m *model) handleCommand(val string) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(sanitizeText(val), " ", 2)
	cmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	switch cmd {
	case "/quit":
		reason := "Disconnected"
		if args != "" {
			reason = args
		}
		globalRoom.Broadcast(ChatMessage{
			Timestamp: time.Now().UTC(),
			Username:  "SYSTEM",
			Color:     "226",
			Text:      fmt.Sprintf("%s left the chat (%s)", m.username, reason),
		})
		close(m.quitChan)
		return m, tea.Quit
	case "/nick":
		if args != "" {
			newName, ok := validateNick(args)
			if !ok {
				m.injectLocalMessage("SYSTEM", "226", "Invalid nickname (use 1–20 letters, numbers, underscore).")
				return m, nil
			}

			oldName := m.username
			m.username = newName

			globalRoom.UpdateUsername(m.sub, m.username)

			userStore.Set(m.login, UserProfile{
				Nick:  m.username,
				Color: m.userColor,
			})

			if !config.Paranoid {
				userStore.Save(config.UserDBPath)
			}

			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  "SYSTEM",
				Color:     "226",
				Text:      fmt.Sprintf("%s is now known as %s", oldName, m.username),
			})
		}
	case "/color":
		if args != "" {
			col, ok := validateColor(args)
			if !ok {
				m.injectLocalMessage("SYSTEM", "226", "Invalid color. Use #RRGGBB or 0–255 ANSI code.")
				return m, nil
			}

			m.userColor = col

			userStore.Set(m.login, UserProfile{
				Nick:  m.username,
				Color: m.userColor,
			})

			if !config.Paranoid {
				userStore.Save(config.UserDBPath)
			}

			m.injectLocalMessage("SYSTEM", "226", "Your color has been updated.")
		}

	case "/me":
		if args != "" {
			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  m.username,
				Color:     m.userColor,
				Text:      args,
				IsAction:  true,
			})
		}

	case "/msg":
		msgParts := strings.SplitN(args, " ", 2)
		if len(msgParts) == 2 {
			target := msgParts[0]
			text := msgParts[1]
			globalRoom.PrivateMessage(m.username, target, ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  fmt.Sprintf("%s -> %s", m.username, target),
				Color:     "199",
				Text:      text,
			})
		}

	case "/who":
		var sb strings.Builder
		sb.WriteString("Users:\n")

		globalRoom.mu.Lock()
		for _, c := range globalRoom.clients {
			sb.WriteString(fmt.Sprintf(" - %s (%s)\n", c.Username, c.IP))
		}
		globalRoom.mu.Unlock()

		m.injectLocalMessage("SYSTEM", "226", sb.String())

	case "/ping":
		m.injectLocalMessage("SYSTEM", "226", "Pong!")

	case "/help":
		helpTxt := `Commands:
		/quit (reason) - Disconnect from the server
		/me [message] - Sends an action message
		/nick [username] - Changes your username
		/msg [username] [message] - Sends a private message
		/who - Lists all connected users
		/ping - Responds with 'Pong!'
		/color [color] - Changes your color
		/help - Display this help message
		`
		m.injectLocalMessage("SYSTEM", "226", helpTxt)

	default:
		m.injectLocalMessage("SYSTEM", "226", "Unknown command. Type /help for a list of commands.")
	}

	return m, nil
}

func (m *model) injectLocalMessage(username, color, text string) {
	lines := strings.Split(text, "\n")

	for i, line := range lines {
		cleanLine := strings.TrimRight(line, "\r")
		if strings.TrimSpace(cleanLine) == "" {
			continue
		}

		m.messages = append(m.messages, ChatMessage{
			Timestamp:    time.Now().UTC(),
			Username:     username,
			Color:        color,
			Text:         cleanLine,
			Continuation: i > 0,
		})
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height + 1
		m.input.SetWidth(m.width - 2)

	case tea.KeyMsg:
		if msg.Key().Code == tea.KeyEsc {
			reason := "Client Interrupt"
			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  "SYSTEM",
				Color:     "226",
				Text:      fmt.Sprintf("%s left the chat (%s)", m.username, reason),
			})
			close(m.quitChan)
			return m, tea.Quit
		}

		if msg.Key().Code == tea.KeyEnter {
			val := strings.TrimSpace(m.input.Value())
			val = sanitizeText(val)
			m.input.SetValue("")

			if val == "" {
				return m, nil
			}

			if strings.HasPrefix(val, "/") {
				return m.handleCommand(val)
			}

			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  m.username,
				Color:     m.userColor,
				Text:      val,
			})

			return m, nil
		}

		switch msg.String() {

		case "ctrl+c":
			reason := "Client Interrupt"
			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  "SYSTEM",
				Color:     "226",
				Text:      fmt.Sprintf("%s left the chat (%s)", m.username, reason),
			})
			close(m.quitChan)
			return m, tea.Quit
		case "up":
			m.scrollOffset++
			return m, nil

		case "down":
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
			return m, nil
		}

	case ChatMessage:
		m.messages = append(m.messages, msg)

		if m.scrollOffset == 0 {
			return m, waitForMessage(m.sub)
		}

		return m, waitForMessage(m.sub)
	}

	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	if m.height == 0 {
		v := tea.NewView("")
		v.AltScreen = true
		return v
	}

	msgHeight := m.height - 3
	if msgHeight < 0 {
		msgHeight = 0
	}

	total := len(m.messages)

	maxOffset := total - msgHeight + 1
	if maxOffset < 0 {
		maxOffset = 0
	}

	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}

	start := total - msgHeight - m.scrollOffset
	end := total - m.scrollOffset + 1

	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if end > total {
		end = total
	}

	visible := m.messages[start:end]

	var view strings.Builder

	for _, msg := range visible {
		view.WriteString(formatMessage(msg, m.width))
		view.WriteString("\n")
	}

	emptyLines := msgHeight - len(visible)
	if emptyLines > 0 {
		view.WriteString(strings.Repeat("\n", emptyLines))
	}

	view.WriteString(borderStyle.Render(strings.Repeat("─", m.width)))
	view.WriteString("\n")

	view.WriteString(m.input.View())

	var vret = tea.NewView(view.String())

	return vret
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	ti := textinput.New()
	ti.Placeholder = "Type a message..."
	ti.Focus()

	login := s.User()

	if strings.ToLower(login) == "system" {
		login = "FakeSystem"
	}

	username := login
	userColor := "15"

	if profile, ok := userStore.Get(login); ok {
		if profile.Nick != "" {
			username = profile.Nick
		}
		if profile.Color != "" {
			userColor = profile.Color
		}
	}
	clientIP := s.RemoteAddr().String()

	subChan := globalRoom.Subscribe(username, clientIP)
	quitChan := make(chan struct{})

	globalRoom.mu.Lock()
	history := make([]ChatMessage, len(globalRoom.history))
	copy(history, globalRoom.history)
	globalRoom.mu.Unlock()

	joinMsg := ChatMessage{
		Timestamp: time.Now().UTC(),
		Username:  "SYSTEM",
		Color:     "226",
		Text:      fmt.Sprintf("Welcome to %s, %s! Type /help for commands.", config.ServerName, username),
	}
	history = append(history, joinMsg)

	globalRoom.Broadcast(ChatMessage{
		Timestamp: time.Now().UTC(),
		Username:  "SYSTEM",
		Color:     "226",
		Text:      fmt.Sprintf("<%s> logged in from %s", username, clientIP),
	})

	m := model{
		login:     login,
		username:  username,
		userColor: userColor,
		input:     ti,
		sub:       subChan,
		messages:  history,
		quitChan:  quitChan,
	}

	go func() {
		<-s.Context().Done()
		globalRoom.Unsubscribe(subChan)

		select {
		case <-quitChan:

		default:
			globalRoom.Broadcast(ChatMessage{
				Timestamp: time.Now().UTC(),
				Username:  "SYSTEM",
				Color:     "226",
				Text:      fmt.Sprintf("%s dropped connection", m.username),
			})
		}
	}()

	return m, []tea.ProgramOption{}
}

func main() {
	loadConfig()
	globalRoom = NewRoom()

	userStore = NewUserStore()
	if err := userStore.Load(config.UserDBPath); err != nil {
		log.Printf("user db load error: %v", err)
	}

	options := []ssh.Option{
		wish.WithAddress(fmt.Sprintf("0.0.0.0:%d", config.Port)),
		wish.WithHostKeyPath(config.IdentityPath),
		wish.WithMiddleware(
			bm.Middleware(teaHandler),
			lm.Middleware(),
		),
	}

	if config.ServerPassword != "" {
		options = append(options, wish.WithPasswordAuth(func(ctx ssh.Context, password string) bool {
			return password == config.ServerPassword
		}))
	}

	s, err := wish.NewServer(options...)
	if err != nil {
		log.Fatalln(err)
	}

	s.SetOption(func(srv *ssh.Server) error {
		srv.ServerConfigCallback = func(ctx ssh.Context) *gossh.ServerConfig {
			cfg := &gossh.ServerConfig{}

			cfg.KeyExchanges = []string{
				"mlkem768x25519-sha256",
			}

			return cfg
		}
		return nil
	})

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Starting %s on port %d...", config.ServerName, config.Port)
	go func() {
		if err = s.ListenAndServe(); err != nil {
			log.Fatalln(err)
		}
	}()

	<-done
	log.Println("Stopping server...")
	if config.Paranoid {
		time.Sleep(100 * time.Millisecond)

		files := []string{config.IdentityPath + ".pub", config.IdentityPath}
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				log.Printf("Could not delete %s: %v", f, err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		log.Fatalln(err)
	}
}
