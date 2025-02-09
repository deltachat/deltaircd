package irckit

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
)

// Channel is a representation of a room in our server
type Channel interface {
	Prefixer

	// ID is a normalized unique identifier for the channel
	ID() string

	// Created returns the time when the Channel was created.
	Created() time.Time

	// Names returns a sorted slice of Nicks in the channel
	Names() []string

	// Users returns a slice of Users in the channel.
	Users() []*User

	// HasUser returns whether a User is in the channel.
	HasUser(*User) bool

	// Invite prompts the User to join the Channel on behalf of Prefixer.
	Invite(from Prefixer, u *User) error

	// SendNamesResponse sends a User messages indicating the current members of the Channel.
	SendNamesResponse(u *User) error

	// Join introduces the User to the channel (handler for JOIN).
	Join(u *User) error

	// BatchJoin
	BatchJoin(users []*User) error

	// Part removes the User from the channel (handler for PART).
	Part(u *User, text string)

	// Message transmits a message from a User to the channel (handler for PRIVMSG).
	Message(u *User, text string)

	// Service returns the service that set the channel
	Service() string

	// Topic sets the topic of the channel (handler for TOPIC).
	Topic(from Prefixer, text string)

	// GetTopic gets the topic of the channel
	GetTopic() string

	// Unlink will disassociate the Channel from its Server.
	Unlink()

	// Len returns the number of Users in the channel.
	Len() int

	// String returns the name of the channel
	String() string

	// Spoof message
	SpoofMessage(from string, text string, maxlen ...int)

	// Spoof notice
	SpoofNotice(from string, text string, maxlen ...int)

	IsPrivate() bool
}

type channel struct {
	created time.Time
	name    string
	server  Server
	id      string
	service string
	private bool

	mu       sync.RWMutex
	topic    string
	usersIdx map[string]*User
}

// NewChannel returns a Channel implementation for a given Server.
func NewChannel(server Server, channelID string, name string, service string, modes map[string]bool) Channel {
	return &channel{
		created:  time.Now(),
		server:   server,
		id:       channelID,
		name:     name,
		service:  service,
		private:  modes["p"],
		usersIdx: make(map[string]*User),
	}
}

func (ch *channel) GetTopic() string {
	return ch.topic
}

func (ch *channel) Prefix() *irc.Prefix {
	return ch.server.Prefix()
}

func (ch *channel) Service() string {
	return ch.service
}

func (ch *channel) String() string {
	return ch.name
}

// Created returns the time when the Channel was created.
func (ch *channel) Created() time.Time {
	return ch.created
}

// ID returns a normalized unique identifier for the channel.
func (ch *channel) ID() string {
	return ID(ch.id)
}

func (ch *channel) Message(from *User, text string) {
	text = wordwrap.String(text, 440)
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		msg := &irc.Message{
			Prefix:        from.Prefix(),
			Command:       irc.PRIVMSG,
			Params:        []string{ch.name},
			Trailing:      l,
			EmptyTrailing: true,
		}

		ch.mu.RLock()

		for _, to := range ch.usersIdx {
			to.Encode(msg)
		}

		ch.mu.RUnlock()
	}
}

// Quit will remove the user from the channel and emit a PART message.
func (ch *channel) Part(u *User, text string) {
	msg := &irc.Message{
		Prefix:   u.Prefix(),
		Command:  irc.PART,
		Params:   []string{ch.name},
		Trailing: text,
	}

	ch.mu.Lock()

	if _, ok := ch.usersIdx[u.ID()]; !ok {
		ch.mu.Unlock()

		u.Encode(&irc.Message{
			Prefix:   ch.Prefix(),
			Command:  irc.ERR_NOTONCHANNEL,
			Params:   []string{ch.name},
			Trailing: "You're not on that channel",
		})

		return
	}

	u.Encode(msg)

	delete(ch.usersIdx, u.ID())

	u.Lock()

	delete(u.channels, ch)

	u.Unlock()

	for _, to := range ch.usersIdx {
		if !to.Ghost {
			to.Encode(msg)
		}
	}

	ch.mu.Unlock()
}

// Unlink will disassociate the Channel from the Server.
func (ch *channel) Unlink() {
	ch.server.UnlinkChannel(ch)
}

// Close will evict all users in the channel.
func (ch *channel) Close() error {
	ch.mu.Lock()

	for _, to := range ch.usersIdx {
		to.Encode(&irc.Message{
			Prefix:  to.Prefix(),
			Command: irc.PART,
			Params:  []string{ch.name},
		})
	}

	ch.usersIdx = make(map[string]*User)

	ch.mu.Unlock()

	return nil
}

// Invite prompts the User to join the Channel on behalf of Prefixer.
func (ch *channel) Invite(from Prefixer, u *User) error {
	return u.Encode(&irc.Message{
		Prefix:  from.Prefix(),
		Command: irc.INVITE,
		Params:  []string{u.Nick, ch.name},
	})
	// TODO: Save state that the user is invited?
}

// Topic sets the topic of the channel (handler for TOPIC).
func (ch *channel) Topic(from Prefixer, text string) {
	ch.mu.RLock()

	// this probably an echo
	if ch.topic == text {
		ch.mu.RUnlock()
		return
	}

	ch.topic = text
	// no newlines in topic
	ch.topic = strings.ReplaceAll(ch.topic, "\n", " ")
	ch.topic = strings.ReplaceAll(ch.topic, "\r", " ")

	msg := &irc.Message{
		Prefix:   from.Prefix(),
		Command:  irc.TOPIC,
		Params:   []string{ch.name},
		Trailing: ch.topic,
	}

	// only send join messages to real users
	for _, to := range ch.usersIdx {
		if !to.Ghost {
			to.Encode(msg)
		}
	}

	ch.mu.RUnlock()
}

// SendNamesResponse sends a User messages indicating the current members of the Channel.
func (ch *channel) SendNamesResponse(u *User) error {
	msgs := []*irc.Message{}
	line := ""
	i := 0

	for _, name := range ch.Names() {
		if i+len(name) < 400 {
			line += name + " "
			i += len(name)
		} else {
			msgs = append(msgs, &irc.Message{
				Prefix:   ch.Prefix(),
				Command:  irc.RPL_NAMREPLY,
				Params:   []string{u.Nick, "=", ch.name},
				Trailing: line,
			})
			line = ""
			line += name + " "
			i = len(name)
		}
	}

	msgs = append(msgs, &irc.Message{
		Prefix:   ch.Prefix(),
		Command:  irc.RPL_NAMREPLY,
		Params:   []string{u.Nick, "=", ch.name},
		Trailing: line,
	})

	msgs = append(msgs, &irc.Message{
		Prefix:   ch.Prefix(),
		Params:   []string{u.Nick, ch.name},
		Command:  irc.RPL_ENDOFNAMES,
		Trailing: "End of /NAMES list.",
	})

	return u.Encode(msgs...)
}

func (ch *channel) BatchJoin(inputusers []*User) error {
	// TODO: Check if user is already here?
	var users []*User

	ch.mu.Lock()

	for _, u := range inputusers {
		if _, exists := ch.usersIdx[u.ID()]; !exists {
			users = append(users, u)
		}
	}

	ch.mu.Unlock()

	for _, u := range users {
		ch.mu.Lock()
		ch.usersIdx[u.ID()] = u
		ch.mu.Unlock()
		u.Lock()
		u.channels[ch] = struct{}{}
		u.Unlock()
	}

	return nil
}

// Join introduces a User to the channel (sends relevant messages, stores).
func (ch *channel) Join(u *User) error {
	// TODO: Check if user is already here?
	ch.mu.Lock()

	if u.ID() == "" {
		ch.mu.Unlock()

		return nil
	}

	if _, exists := ch.usersIdx[u.ID()]; exists {
		ch.mu.Unlock()
		return nil
	}

	ch.usersIdx[u.ID()] = u

	ch.mu.Unlock()
	u.Lock()

	u.channels[ch] = struct{}{}

	u.Unlock()

	// speed up &users join
	if ch.name == "&users" && u.Ghost {
		return nil
	}

	msg := &irc.Message{
		Prefix:  u.Prefix(),
		Command: irc.JOIN,
		Params:  []string{ch.name},
	}

	// send regular users a notification of the join
	ch.mu.RLock()

	for _, to := range ch.usersIdx {
		// only send join messages to real users
		if !to.Ghost {
			to.Encode(msg)
		}
	}

	ch.mu.RUnlock()

	ch.SendNamesResponse(u)

	return nil
}

func (ch *channel) HasUser(u *User) bool {
	ch.mu.RLock()

	_, ok := ch.usersIdx[u.ID()]

	ch.mu.RUnlock()

	return ok
}

// Users returns an unsorted slice of users who are in the channel.
func (ch *channel) Users() []*User {
	ch.mu.RLock()

	users := make([]*User, 0, len(ch.usersIdx))
	for _, u := range ch.usersIdx {
		users = append(users, u)
	}

	ch.mu.RUnlock()

	return users
}

// Names returns a sorted slice of Nick strings of users who are in the channel.
func (ch *channel) Names() []string {
	users := ch.Users()
	names := make([]string, 0, len(users))

	for _, u := range users {
		names = append(names, u.Nick)
	}

	// TODO: Append in sorted order?
	sort.Strings(names)

	return names
}

// Len returns the number of users in the channel.
func (ch *channel) Len() int {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	return len(ch.usersIdx)
}

func (ch *channel) Spoof(from string, text string, cmd string, maxlen ...int) {
	if len(maxlen) == 0 {
		text = wordwrap.String(text, 440)
	} else {
		text = wordwrap.String(text, maxlen[0])
	}
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		msg := &irc.Message{
			Prefix:        &irc.Prefix{Name: from, User: from, Host: from},
			Command:       cmd,
			Params:        []string{ch.name},
			Trailing:      l,
			EmptyTrailing: true,
		}

		ch.mu.RLock()

		for _, to := range ch.usersIdx {
			to.Encode(msg)
		}

		ch.mu.RUnlock()
	}
}

func (ch *channel) SpoofMessage(from string, text string, maxlen ...int) {
	if len(maxlen) == 0 {
		ch.Spoof(from, text, irc.PRIVMSG, 440)
	} else {
		ch.Spoof(from, text, irc.PRIVMSG, maxlen[0])
	}
}

func (ch *channel) SpoofNotice(from string, text string, maxlen ...int) {
	if len(maxlen) == 0 {
		ch.Spoof(from, text, irc.NOTICE, 440)
	} else {
		ch.Spoof(from, text, irc.NOTICE, maxlen[0])
	}
}

func (ch *channel) IsPrivate() bool {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	return ch.private
}
