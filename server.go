package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	irc "gopkg.in/sorcix/irc.v2"
)

type Config struct {
	SSLCert       string
	SSLKey        string
	ProfilingPort int
}

type Server struct {
	config       *Config
	created      int64
	clients      *sync.Map
	channels     *sync.Map
	odyssey      *os.File
	odysseymutex *sync.RWMutex

	restartplain chan bool
	restartssl   chan bool

	*sync.RWMutex
}

func NewServer() *Server {
	s := &Server{}
	s.config = &Config{}
	s.created = time.Now().Unix()
	s.clients = new(sync.Map)
	s.channels = new(sync.Map)
	s.odysseymutex = new(sync.RWMutex)

	s.restartplain = make(chan bool, 1)
	s.restartssl = make(chan bool, 1)
	s.RWMutex = new(sync.RWMutex)

	return s
}

func (s *Server) getAnonymousPrefix(i int) *irc.Prefix {
	prefix := anonymous
	if i > 1 {
		prefix.Name += fmt.Sprintf("%d", i)
	}
	return &prefix
}

func (s *Server) getChannel(channel string) *Channel {
	if ch, ok := s.channels.Load(channel); ok {
		return ch.(*Channel)
	}

	return nil
}

func (s *Server) getChannels(client string) map[string]*Channel {
	channels := make(map[string]*Channel)
	s.channels.Range(func(k, v interface{}) bool {
		key := k.(string)
		channel := v.(*Channel)
		if s.inChannel(key, client) {
			channels[key] = channel
		}

		return true
	})

	return channels
}

func (s *Server) getClient(client string) *Client {
	if cl, ok := s.clients.Load(client); ok {
		return cl.(*Client)
	}

	return nil
}

func (s *Server) getClients(channel string) map[string]*Client {
	clients := make(map[string]*Client)

	ch := s.getChannel(channel)

	ch.clients.Range(func(k, v interface{}) bool {
		cl := s.getClient(k.(string))
		if cl != nil {
			clients[cl.identifier] = cl
		}

		return true
	})

	return clients
}

func (s *Server) channelExists(channel string) bool {
	if _, ok := s.channels.Load(channel); ok {
		return true
	}

	return false
}

func (s *Server) inChannel(channel string, client string) bool {
	ch := s.getChannel(channel)
	if ch != nil {
		_, ok := ch.clients.Load(client)
		return ok
	}

	return false
}

func (s *Server) joinChannel(channel string, client string) {
	if s.inChannel(channel, client) {
		return // Already in channel
	}

	ch := s.getChannel(channel)
	cl := s.getClient(client)

	if cl == nil {
		return
	}

	if ch == nil {
		ch = NewChannel(channel)
		s.channels.Store(channel, ch)
	} else if ch.hasMode("z") && !cl.ssl {
		cl.sendNotice("Unable to join " + channel + ": SSL connections only (channel mode +z)")
		return
	}

	ch.clients.Store(client, s.getClientCount(channel, client)+1)
	cl.write(&irc.Message{cl.getPrefix(), irc.JOIN, []string{channel}})

	s.sendNames(channel, client)
	s.updateClientCount(channel, client)
	s.sendTopic(channel, client, false)
}

func (s *Server) partChannel(channel string, client string, reason string) {
	ch := s.getChannel(channel)
	cl := s.getClient(client)

	if cl == nil || !s.inChannel(channel, client) {
		return
	}

	cl.write(&irc.Message{cl.getPrefix(), irc.PART, []string{channel, reason}})
	ch.clients.Delete(client)

	s.updateClientCount(channel, client)
}

func (s *Server) partAllChannels(client string) {
	for channelname := range s.getChannels(client) {
		s.partChannel(channelname, client, "")
	}
}

func (s *Server) enforceModes(channel string) {
	ch := s.getChannel(channel)

	if ch != nil && ch.hasMode("z") {
		for client, cl := range s.getClients(channel) {
			if !cl.ssl {
				s.partChannel(channel, client, "Only SSL connections are allowed in this channel")
			}
		}
	}
}

func (s *Server) getClientCount(channel string, client string) int {
	ch := s.getChannel(channel)
	cl := s.getClient(client)

	if ch == nil || cl == nil {
		return 0
	}

	ccount := 0
	ch.clients.Range(func(k, v interface{}) bool {
		ccount++
		return true
	})

	if (ch.hasMode("c") || cl.hasMode("c")) && ccount >= 2 {
		return 2
	}

	return ccount
}

func (s *Server) updateClientCount(channel string, client string) {
	ch := s.getChannel(channel)

	if ch == nil {
		return
	}

	ch.clients.Range(func(k, v interface{}) bool {
		cl := s.getClient(k.(string))
		ccount := v.(int)

		if cl == nil {
			return true
		} else if client != "" && ch.hasMode("D") && cl.identifier != client {
			return true
		}

		chancount := s.getClientCount(channel, cl.identifier)

		if ccount < chancount {
			for i := ccount; i < chancount; i++ {
				cl.write(&irc.Message{s.getAnonymousPrefix(i), irc.JOIN, []string{channel}})
			}

			ch.clients.Store(cl.identifier, chancount)
		} else if ccount > chancount {
			for i := ccount; i > chancount; i-- {
				cl.write(&irc.Message{s.getAnonymousPrefix(i - 1), irc.PART, []string{channel}})
			}
		} else {
			return true
		}

		ch.clients.Store(cl.identifier, chancount)

		return true
	})
}

func (s *Server) sendNames(channel string, clientname string) {
	if s.inChannel(channel, clientname) {
		cl := s.getClient(clientname)

		if cl == nil {
			return
		}

		names := []string{}
		if cl.capHostInNames {
			names = append(names, cl.getPrefix().String())
		} else {
			names = append(names, cl.nick)
		}

		ccount := s.getClientCount(channel, clientname)
		for i := 1; i < ccount; i++ {
			if cl.capHostInNames {
				names = append(names, s.getAnonymousPrefix(i).String())
			} else {
				names = append(names, s.getAnonymousPrefix(i).Name)
			}
		}

		cl.write(&irc.Message{&anonirc, irc.RPL_NAMREPLY, []string{"=", channel, strings.Join(names, " ")}})
		cl.write(&irc.Message{&anonirc, irc.RPL_ENDOFNAMES, []string{channel, "End of /NAMES list."}})
	}
}

func (s *Server) sendTopic(channel string, client string, changed bool) {
	if !s.inChannel(channel, client) {
		return
	}

	ch := s.getChannel(channel)
	cl := s.getClient(client)

	if ch == nil || cl == nil {
		return
	}

	if ch.topic != "" {
		tprefix := anonymous
		tcommand := irc.TOPIC
		if !changed {
			tprefix = anonirc
			tcommand = irc.RPL_TOPIC
		}
		cl.write(&irc.Message{&tprefix, tcommand, []string{channel, ch.topic}})

		if !changed {
			cl.write(&irc.Message{&anonirc, strings.Join([]string{irc.RPL_TOPICWHOTIME, cl.nick, channel, anonymous.Name, fmt.Sprintf("%d", ch.topictime)}, " "), nil})
		}
	}
}

func (s *Server) handleTopic(channel string, client string, topic string) {
	ch := s.getChannel(channel)
	cl := s.getClient(client)

	if ch == nil || cl == nil {
		return
	}

	if !s.inChannel(channel, client) {
		cl.sendNotice("Invalid use of TOPIC")
		return
	}

	if topic != "" {
		ch.topic = topic
		ch.topictime = time.Now().Unix()

		ch.clients.Range(func(k, v interface{}) bool {
			s.sendTopic(channel, k.(string), true)
			return true
		})
	} else {
		s.sendTopic(channel, client, false)
	}
}

func (s *Server) handleMode(c *Client, params []string) {
	if len(params) == 0 || len(params[0]) == 0 {
		c.sendNotice("Invalid use of MODE")
		return
	}

	if params[0][0] == '#' {
		ch := s.getChannel(params[0])

		if ch == nil {
			return
		}

		if len(params) == 1 || params[1] == "" {
			c.write(&irc.Message{&anonirc, strings.Join([]string{irc.RPL_CHANNELMODEIS, c.nick, params[0], ch.printModes(ch.getModes(), nil)}, " "), []string{}})

			// Send channel creation time
			c.write(&irc.Message{&anonirc, strings.Join([]string{"329", c.nick, params[0], fmt.Sprintf("%d", int32(ch.created))}, " "), []string{}})
		} else if len(params) > 1 && len(params[1]) > 0 && (params[1][0] == '+' || params[1][0] == '-') {
			lastmodes := make(map[string]string)
			for m, mv := range ch.getModes() {
				lastmodes[m] = mv
			}

			if params[1][0] == '+' {
				ch.addModes(params[1][1:])
			} else {
				ch.removeModes(params[1][1:])
			}
			s.enforceModes(params[0])

			if !reflect.DeepEqual(ch.getModes(), lastmodes) {
				// TODO: Check if local modes were set/unset, only send changes to local client
				addedmodes, removedmodes := ch.diffModes(lastmodes)

				resendusercount := false
				if _, ok := addedmodes["c"]; ok {
					resendusercount = true
				}
				if _, ok := removedmodes["c"]; ok {
					resendusercount = true
				}
				if _, ok := removedmodes["D"]; ok {
					resendusercount = true
				}

				if len(addedmodes) == 0 && len(removedmodes) == 0 {
					addedmodes = c.getModes()
				}

				ch.clients.Range(func(k, v interface{}) bool {
					cl := s.getClient(k.(string))
					if cl != nil {
						cl.write(&irc.Message{&anonymous, irc.MODE, []string{params[0], ch.printModes(addedmodes, removedmodes)}})
					}

					return true
				})

				if resendusercount {
					s.updateClientCount(params[0], c.identifier)
				}
			}
		}
	} else {
		if len(params) == 1 || params[1] == "" {
			c.write(&irc.Message{&anonirc, strings.Join([]string{irc.RPL_UMODEIS, c.nick, c.printModes(c.getModes(), nil)}, " "), []string{}})
			return
		}

		lastmodes := c.getModes()

		if len(params) > 1 && len(params[1]) > 0 && (params[1][0] == '+' || params[1][0] == '-') {
			if params[1][0] == '+' {
				c.addModes(params[1][1:])
			} else {
				c.removeModes(params[1][1:])
			}
		}

		if !reflect.DeepEqual(c.modes, lastmodes) {
			addedmodes, removedmodes := c.diffModes(lastmodes)

			resendusercount := false
			if _, ok := addedmodes["c"]; ok {
				resendusercount = true
			}
			if _, ok := removedmodes["c"]; ok {
				resendusercount = true
			}
			if _, ok := removedmodes["D"]; ok {
				resendusercount = true
			}

			if len(addedmodes) == 0 && len(removedmodes) == 0 {
				addedmodes = c.getModes()
			}

			c.write(&irc.Message{&anonirc, strings.Join([]string{irc.MODE, c.nick}, " "), []string{c.printModes(addedmodes, removedmodes)}})

			if resendusercount {
				for ch := range s.getChannels(c.identifier) {
					s.updateClientCount(ch, c.identifier)
				}
			}
		}
	}
}

func (s *Server) handlePrivmsg(channel string, client string, message string) {
	if !s.inChannel(channel, client) {
		return // Not in channel
	}

	ch := s.getChannel(channel)

	if ch == nil {
		return
	}

	s.updateClientCount(channel, "")

	ch.clients.Range(func(k, v interface{}) bool {
		cl := s.getClient(k.(string))
		if cl != nil && cl.identifier != client {
			cl.write(&irc.Message{&anonymous, irc.PRIVMSG, []string{channel, message}})
		}

		return true
	})
}

func (s *Server) handleRead(c *Client) {
	for {
		c.conn.SetDeadline(time.Now().Add(300 * time.Second))

		if _, ok := s.clients.Load(c.identifier); !ok {
			s.killClient(c)
			return
		}

		msg, err := c.reader.Decode()
		if msg == nil || err != nil {
			s.killClient(c)
			return
		}
		if len(msg.Command) >= 4 && msg.Command[0:4] != irc.PING && msg.Command[0:4] != irc.PONG {
			log.Println(c.identifier, "<-", fmt.Sprintf("%s", msg))
		}
		if msg.Command == irc.CAP && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0] == irc.CAP_LS {
			c.write(&irc.Message{&anonirc, irc.CAP, []string{msg.Params[0], "userhost-in-names"}})
		} else if msg.Command == irc.CAP && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0] == irc.CAP_REQ {
			if strings.Contains(msg.Trailing(), "userhost-in-names") {
				c.capHostInNames = true
			}
			c.write(&irc.Message{&anonirc, irc.CAP, []string{irc.CAP_ACK, msg.Trailing()}})
		} else if msg.Command == irc.CAP && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0] == irc.CAP_LIST {
			caps := []string{}
			if c.capHostInNames {
				caps = append(caps, "userhost-in-names")
			}
			c.write(&irc.Message{&anonirc, irc.CAP, []string{msg.Params[0], strings.Join(caps, " ")}})
		} else if msg.Command == irc.PING {
			c.write(&irc.Message{&anonirc, irc.PONG + " AnonIRC", []string{msg.Trailing()}})
		} else if msg.Command == irc.NICK && c.nick == "*" && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0] != "" && msg.Params[0] != "*" {
			c.nick = strings.Trim(msg.Params[0], "\"")
		} else if msg.Command == irc.USER && c.user == "" && len(msg.Params) >= 3 && msg.Params[0] != "" && msg.Params[2] != "" {
			c.user = strings.Trim(msg.Params[0], "\"")
			c.host = strings.Trim(msg.Params[2], "\"")

			c.write(&irc.Message{&anonirc, irc.RPL_WELCOME, []string{"Welcome to AnonIRC " + c.getPrefix().String()}})
			c.write(&irc.Message{&anonirc, irc.RPL_YOURHOST, []string{"Your host is AnonIRC, running version AnonIRCd"}})
			c.write(&irc.Message{&anonirc, irc.RPL_CREATED, []string{fmt.Sprintf("This server was created %s", time.Unix(s.created, 0).UTC())}})
			c.write(&irc.Message{&anonirc, strings.Join([]string{irc.RPL_MYINFO, c.nick, "AnonIRC AnonIRCd", CLIENT_MODES, CHANNEL_MODES, CHANNEL_MODES_ARG}, " "), []string{}})

			motdsplit := strings.Split(motd, "\n")
			for i, motdmsg := range motdsplit {
				var motdcode string
				if i == 0 {
					motdcode = irc.RPL_MOTDSTART
				} else if i < len(motdsplit)-1 {
					motdcode = irc.RPL_MOTD
				} else {
					motdcode = irc.RPL_ENDOFMOTD
				}
				c.write(&irc.Message{&anonirc, motdcode, []string{"  " + motdmsg}})
			}

			s.joinChannel("#", c.identifier)
		} else if msg.Command == irc.WHOIS && len(msg.Params) > 0 && len(msg.Params[0]) >= len(anonymous.Name) && strings.ToLower(msg.Params[0][:len(anonymous.Name)]) == strings.ToLower(anonymous.Name) {
			go func() {
				whoisindex := 1
				if len(msg.Params[0]) > len(anonymous.Name) {
					whoisindex, err = strconv.Atoi(msg.Params[0][len(anonymous.Name):])
					if err != nil || whoisindex <= 1 {
						return
					}
				}

				whoisnick := anonymous.Name
				if whoisindex > 1 {
					whoisnick += strconv.Itoa(whoisindex)
				}

				easteregg := s.readOdyssey(whoisindex)
				if easteregg == "" {
					easteregg = "I am the owner of my actions, heir of my actions, actions are the womb (from which I have sprung), actions are my relations, actions are my protection. Whatever actions I do, good or bad, of these I shall become the heir."
				}

				c.write(&irc.Message{&anonirc, irc.RPL_AWAY, []string{whoisnick, easteregg}})
				c.write(&irc.Message{&anonirc, irc.RPL_ENDOFWHOIS, []string{whoisnick, "End of /WHOIS list."}})
			}()
		} else if msg.Command == irc.AWAY {
			if len(msg.Params) > 0 {
				c.write(&irc.Message{&anonirc, irc.RPL_NOWAWAY, []string{"You have been marked as being away"}})
			} else {
				c.write(&irc.Message{&anonirc, irc.RPL_UNAWAY, []string{"You are no longer marked as being away"}})
			}
		} else if msg.Command == irc.LIST {
			chans := make(map[string]int)
			s.channels.Range(func(k, v interface{}) bool {
				key := k.(string)
				ch := v.(*Channel)

				if ch != nil && !ch.hasMode("p") && !ch.hasMode("s") {
					chans[key] = s.getClientCount(key, c.identifier)
				}

				return true
			})

			c.write(&irc.Message{&anonirc, irc.RPL_LISTSTART, []string{"Channel", "Users Name"}})
			for _, pl := range sortMapByValues(chans) {
				ch := s.getChannel(pl.Key)

				c.write(&irc.Message{&anonirc, irc.RPL_LIST, []string{pl.Key, strconv.Itoa(pl.Value), "[" + ch.printModes(ch.getModes(), nil) + "] " + ch.topic}})
			}
			c.write(&irc.Message{&anonirc, irc.RPL_LISTEND, []string{"End of /LIST"}})
		} else if msg.Command == irc.JOIN && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0][0] == '#' {
			for _, channel := range strings.Split(msg.Params[0], ",") {
				s.joinChannel(channel, c.identifier)
			}
		} else if msg.Command == irc.NAMES && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0][0] == '#' {
			for _, channel := range strings.Split(msg.Params[0], ",") {
				s.sendNames(channel, c.identifier)
			}
		} else if msg.Command == irc.WHO && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0][0] == '#' {
			var ccount int
			for _, channel := range strings.Split(msg.Params[0], ",") {
				if s.inChannel(channel, c.identifier) {
					ccount = s.getClientCount(channel, c.identifier)
					for i := 0; i < ccount; i++ {
						var prfx *irc.Prefix
						if i == 0 {
							prfx = c.getPrefix()
						} else {
							prfx = s.getAnonymousPrefix(i)
						}

						c.write(&irc.Message{&anonirc, irc.RPL_WHOREPLY, []string{channel, prfx.User, prfx.Host, "AnonIRC", prfx.Name, "H", "0 " + anonymous.Name}})
					}
					c.write(&irc.Message{&anonirc, irc.RPL_ENDOFWHO, []string{channel, "End of /WHO list."}})
				}
			}
		} else if msg.Command == irc.MODE {
			if len(msg.Params) == 2 && msg.Params[0][0] == '#' && msg.Params[1] == "b" {
				c.write(&irc.Message{&anonirc, irc.RPL_ENDOFBANLIST, []string{msg.Params[0], "End of Channel Ban List"}})
			} else {
				s.handleMode(c, msg.Params)
			}
		} else if msg.Command == irc.TOPIC && len(msg.Params) > 0 && len(msg.Params[0]) > 0 {
			s.handleTopic(msg.Params[0], c.identifier, msg.Trailing())
		} else if msg.Command == irc.PRIVMSG && len(msg.Params) > 0 && len(msg.Params[0]) > 0 {
			s.handlePrivmsg(msg.Params[0], c.identifier, msg.Trailing())
		} else if msg.Command == irc.PART && len(msg.Params) > 0 && len(msg.Params[0]) > 0 && msg.Params[0][0] == '#' {
			for _, channel := range strings.Split(msg.Params[0], ",") {
				s.partChannel(channel, c.identifier, "")
			}
		} else if msg.Command == irc.QUIT {
			s.killClient(c)
		}
	}
}

func (s *Server) handleWrite(c *Client) {
	for msg := range c.writebuffer {
		if msg == nil {
			return
		}

		addnick := false
		if _, err := strconv.Atoi(msg.Command); err == nil {
			addnick = true
		} else if msg.Command == irc.CAP {
			addnick = true
		}

		if addnick {
			msg.Params = append([]string{c.nick}, msg.Params...)
		}

		if len(msg.Command) >= 4 && msg.Command[0:4] != irc.PING && msg.Command[0:4] != irc.PONG {
			log.Println(c.identifier, "->", msg)
		}
		c.writer.Encode(msg)
	}
}

func (s *Server) handleConnection(conn net.Conn, ssl bool) {
	defer conn.Close()
	var identifier string

	for {
		identifier = randomIdentifier()
		if _, ok := s.clients.Load(identifier); !ok {
			break
		}
	}

	client := NewClient(identifier, conn, ssl)
	s.clients.Store(client.identifier, client)

	go s.handleWrite(client)
	s.handleRead(client) // Block until the connection is closed

	s.killClient(client)
	close(client.writebuffer)
	s.clients.Delete(identifier)
}

func (s *Server) killClient(c *Client) {
	if c.state == ENTITY_STATE_TERMINATING {
		return
	}
	c.state = ENTITY_STATE_TERMINATING

	c.write(nil)
	c.conn.Close()
	if _, ok := s.clients.Load(c.identifier); ok {
		s.partAllChannels(c.identifier)
	}
}

func (s *Server) listenPlain() {
	for {
		listen, err := net.Listen("tcp", ":6667")
		if err != nil {
			log.Printf("Failed to listen: %v", err)
			time.Sleep(1 * time.Minute)
			continue
		}
		log.Println("Listening on 6667")

	accept:
		for {
			select {
			case _ = <-s.restartplain:
				break accept
			default:
				conn, err := listen.Accept()
				if err != nil {
					log.Println("Error accepting connection:", err)
					continue
				}
				go s.handleConnection(conn, true)
			}
		}
		listen.Close()
	}
}

func (s *Server) listenSSL() {
	for {
		if s.config.SSLCert == "" {
			time.Sleep(1 * time.Minute)
			return // SSL is disabled
		}

		cert, err := tls.LoadX509KeyPair(s.config.SSLCert, s.config.SSLKey)
		if err != nil {
			log.Printf("Failed to load SSL certificate: %v", err)
			time.Sleep(1 * time.Minute)
			continue
		}

		listen, err := tls.Listen("tcp", ":6697", &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			log.Printf("Failed to listen: %v", err)
			time.Sleep(1 * time.Minute)
			continue
		}
		log.Println("Listening on +6697")

	accept:
		for {
			select {
			case _ = <-s.restartssl:
				break accept
			default:
				conn, err := listen.Accept()
				if err != nil {
					log.Println("Error accepting connection:", err)
					continue
				}
				go s.handleConnection(conn, true)
			}
		}
		listen.Close()
	}
}

func (s *Server) pingClients() {
	for {
		s.clients.Range(func(k, v interface{}) bool {
			cl := v.(*Client)
			if cl != nil {
				cl.write(&irc.Message{nil, irc.PING, []string{fmt.Sprintf("anonirc%d%d", int32(time.Now().Unix()), rand.Intn(1000))}})
			}

			return true
		})
		time.Sleep(90 * time.Second)
	}
}

func (s *Server) loadConfig() {
	if _, err := os.Stat("anonircd.conf"); err == nil {
		if _, err := toml.DecodeFile("anonircd.conf", &s.config); err != nil {
			log.Fatalf("Failed to read anonircd.conf: %v", err)
		}
	}
}

func (s *Server) reload() {
	log.Println("Reloading configuration")
	s.loadConfig()
	s.restartplain <- true
	s.restartssl <- true
}

func (s *Server) readOdyssey(line int) string {
	s.odysseymutex.Lock()
	defer s.odysseymutex.Unlock()

	scanner := bufio.NewScanner(s.odyssey)
	currentline := 1
	for scanner.Scan() {
		if currentline == line {
			s.odyssey.Seek(0, 0)
			return scanner.Text()
		}

		currentline++
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Failed to read ODYSSEY: %v", err)
		return ""
	}

	s.odyssey.Seek(0, 0)
	return ""
}

func (s *Server) startProfiling() {
	if s.config.ProfilingPort > 0 {
		http.ListenAndServe(fmt.Sprintf("localhost:%d", s.config.ProfilingPort), nil)
	}
}

func (s *Server) listen() {
	go s.listenPlain()
	go s.listenSSL()

	s.pingClients()
}
