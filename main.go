package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type Room struct {
	ID           string
	Clients      map[string]*Client
	clientsMu    sync.Mutex
	LastActivity time.Time
}

type Client struct {
	ID       string
	Conn     *websocket.Conn
	RoomID   string
	Username string
	JoinedAt time.Time
}

type SignalMessage struct {
	RoomID    string                   `json:"roomId"`
	ClientID  string                   `json:"clientId"`
	Username  string                   `json:"username,omitempty"`
	TargetID  string                   `json:"targetId,omitempty"`
	Type      string                   `json:"type"` // "offer", "answer", "ice-candidate", "join", "leave", "room-state"
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Clients   []ClientInfo             `json:"clients,omitempty"` // For room-state messages
}

type ClientInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

var (
	rooms    = make(map[string]*Room)
	roomsMu  sync.Mutex
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Permisif untuk pengembangan
		},
	}
)

func main() {
	// Start room cleanup routine
	go cleanupInactiveRooms()

	// Endpoint statis untuk frontend
	http.Handle("/", http.FileServer(http.Dir("./static")))

	// Endpoint untuk mendapatkan konfigurasi ICE server
	http.HandleFunc("/ice-servers", handleICEServers)

	// Endpoint untuk WebSocket signaling
	http.HandleFunc("/ws", handleWebSocket)

	// Endpoint untuk membuat ruangan baru
	http.HandleFunc("/api/create-room", handleCreateRoom)
	
	// Endpoint untuk mendapatkan daftar ruangan aktif
	http.HandleFunc("/api/list-rooms", handleListRooms)

	log.Println("Server dimulai di port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleICEServers(w http.ResponseWriter, r *http.Request) {
	iceServers := []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
		// Tambahkan TURN server jika dibutuhkan
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(iceServers)
}

func handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Buat ID ruangan (sebaiknya gunakan UUID atau ID yang lebih aman)
	roomID := generateRoomID()

	roomsMu.Lock()
	rooms[roomID] = &Room{
		ID:           roomID,
		Clients:      make(map[string]*Client),
		LastActivity: time.Now(),
	}
	roomsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"roomId": roomID})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading to WebSocket:", err)
		return
	}

	// Get query parameters
	query := r.URL.Query()
	roomID := query.Get("roomId")
	username := query.Get("username")

	// Use default username if not provided
	if username == "" {
		username = "User-" + uuid.New().String()[:8]
	}

	// Generasi ID klien (lebih baik gunakan UUID)
	clientID := generateClientID()

	// Create client object
	client := &Client{
		ID:       clientID,
		Conn:     conn,
		RoomID:   roomID,
		Username: username,
		JoinedAt: time.Now(),
	}

	// Add client to room
	roomsMu.Lock()
	room, exists := rooms[roomID]
	if !exists {
		// Jika ruangan tidak ada, buat ruangan baru
		room = &Room{
			ID:           roomID,
			Clients:      make(map[string]*Client),
			LastActivity: time.Now(),
		}
		rooms[roomID] = room
	}
	roomsMu.Unlock()

	// Add client to room
	room.clientsMu.Lock()
	room.Clients[clientID] = client
	room.LastActivity = time.Now()
	room.clientsMu.Unlock()

	// Send room state to new client
	sendRoomState(room, clientID)

	// Notify other clients about new joiner
	notifyClientJoined(room, client)

	// Process messages in a loop
	handleClientMessages(client, room)

	// Cleanup when client disconnects
	log.Printf("Client %s disconnected from room %s", clientID, roomID)
	cleanupClient(client)
}

func handleClientMessages(client *Client, room *Room) {
	defer client.Conn.Close()

	for {
		var msg SignalMessage
		err := client.Conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading message from client %s: %v", client.ID, err)
			break
		}

		// Update room activity timestamp
		room.LastActivity = time.Now()

		// Set client ID and room ID in the message
		msg.ClientID = client.ID
		msg.RoomID = client.RoomID
		msg.Username = client.Username

		// Handle message based on type
		switch msg.Type {
		case "offer", "answer", "ice-candidate":
			// Forward SDP or ICE candidate to specific target
			forwardToClient(room, msg)
		default:
			// Process other message types if needed
			log.Printf("Received message of type %s from client %s", msg.Type, client.ID)
		}
	}
}

func forwardToClient(room *Room, msg SignalMessage) {
	if msg.TargetID == "" {
		return
	}

	room.clientsMu.Lock()
	targetClient, exists := room.Clients[msg.TargetID]
	room.clientsMu.Unlock()

	if exists {
		err := targetClient.Conn.WriteJSON(msg)
		if err != nil {
			log.Printf("Error forwarding message to client %s: %v", msg.TargetID, err)
		}
	}
}

func sendRoomState(room *Room, clientID string) {
	room.clientsMu.Lock()
	defer room.clientsMu.Unlock()

	client, exists := room.Clients[clientID]
	if !exists {
		return
	}
	log.Printf("Client %s disconnected from room %s", clientID, room.ID)

	// Create list of clients in the room (excluding self)
	var clients []ClientInfo
	for id, c := range room.Clients {
		if id != clientID {
			clients = append(clients, ClientInfo{
				ID:       id,
				Username: c.Username,
			})
		}
	}

	// Send room state message to the client
	stateMsg := SignalMessage{
		Type:    "room-state",
		RoomID:  room.ID,
		Clients: clients,
	}

	err := client.Conn.WriteJSON(stateMsg)
	if err != nil {
		log.Printf("Error sending room state to client %s: %v", clientID, err)
	}
}

func notifyClientJoined(room *Room, newClient *Client) {
	joinMsg := SignalMessage{
		Type:     "join",
		RoomID:   room.ID,
		ClientID: newClient.ID,
		Username: newClient.Username,
	}

	room.clientsMu.Lock()
	defer room.clientsMu.Unlock()

	// Broadcast join message to all other clients
	for id, client := range room.Clients {
		if id != newClient.ID {
			err := client.Conn.WriteJSON(joinMsg)
			if err != nil {
				log.Printf("Error notifying client %s about new joiner: %v", id, err)
			}
		}
	}
}

func notifyClientLeft(room *Room, clientID string) {
	leaveMsg := SignalMessage{
		Type:     "leave",
		RoomID:   room.ID,
		ClientID: clientID,
	}

	room.clientsMu.Lock()
	defer room.clientsMu.Unlock()

	// Broadcast leave message to all other clients
	for id, client := range room.Clients {
		if id != clientID {
			err := client.Conn.WriteJSON(leaveMsg)
			if err != nil {
				log.Printf("Error notifying client %s about leaving: %v", id, err)
			}
		}
	}
}

func cleanupClient(client *Client) {
	roomID := client.RoomID
	clientID := client.ID

	roomsMu.Lock()
	room, exists := rooms[roomID]
	if !exists {
		roomsMu.Unlock()
		return
	}
	roomsMu.Unlock()

	// Notify other clients about the leaving client
	notifyClientLeft(room, clientID)

	// Remove client from room
	room.clientsMu.Lock()
	delete(room.Clients, clientID)
	clientCount := len(room.Clients)
	room.clientsMu.Unlock()

	// If room is empty, consider removing it
	if clientCount == 0 {
		roomsMu.Lock()
		// Double check if room is still empty before deleting
		room.clientsMu.Lock()
		if len(room.Clients) == 0 {
			delete(rooms, roomID)
			log.Printf("Removed empty room %s", roomID)
		}
		room.clientsMu.Unlock()
		roomsMu.Unlock()
	}
}

func cleanupInactiveRooms() {
	const cleanupInterval = 5 * time.Minute
	const roomTimeout = 1 * time.Hour

	for {
		time.Sleep(cleanupInterval)

		roomsMu.Lock()
		now := time.Now()

		for id, room := range rooms {
			room.clientsMu.Lock()
			if len(room.Clients) == 0 && now.Sub(room.LastActivity) > roomTimeout {
				delete(rooms, id)
				log.Printf("Cleaned up inactive room: %s", id)
			}
			room.clientsMu.Unlock()
		}

		roomsMu.Unlock()
	}
}

// Fungsi untuk mendapatkan daftar ruangan aktif
func handleListRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	type RoomInfo struct {
		ID        string `json:"id"`
		UserCount int    `json:"userCount"`
	}

	roomsMu.Lock()
	defer roomsMu.Unlock()

	// Filter rooms with at least one active user and create response
	activeRooms := []RoomInfo{}
	for id, room := range rooms {
		room.clientsMu.Lock()
		clientCount := len(room.Clients)
		room.clientsMu.Unlock()

		if clientCount > 0 {
			activeRooms = append(activeRooms, RoomInfo{
				ID:        id,
				UserCount: clientCount,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"rooms": activeRooms,
	})
}

// Fungsi helper untuk menghasilkan ID
func generateRoomID() string {
	// Implementasikan logika untuk menghasilkan ID unik
	return "room-" + uuid.New().String()
}

func generateClientID() string {
	// Generate unique client ID using UUID
	return "client-" + uuid.New().String()
}
