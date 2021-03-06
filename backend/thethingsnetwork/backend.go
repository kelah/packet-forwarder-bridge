package thethingsnetwork

import (
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/TheThingsNetwork/ttn/api"
	pb_discovery "github.com/TheThingsNetwork/ttn/api/discovery"
	pb_gateway "github.com/TheThingsNetwork/ttn/api/gateway"
	pb_protocol "github.com/TheThingsNetwork/ttn/api/protocol"
	pb_lorawan "github.com/TheThingsNetwork/ttn/api/protocol/lorawan"
	pb_router "github.com/TheThingsNetwork/ttn/api/router"
	"github.com/TheThingsNetwork/ttn/core/types"
	"github.com/brocaar/loraserver/api/gw"
	"github.com/brocaar/lorawan"
	"github.com/brocaar/lorawan/band"
	metrics "github.com/rcrowley/go-metrics"
	"google.golang.org/grpc"
)

func init() {
	api.SetLogger(WrapLogrus())
}

type gtwConf struct {
	id    string
	token string
}

type gtw struct {
	id       string
	token    string
	client   pb_router.RouterClientForGateway
	uplink   pb_router.UplinkStream
	status   pb_router.GatewayStatusStream
	downlink pb_router.DownlinkStream
	rxRate   metrics.EWMA
}

func (g *gtw) Close() {
	if g.uplink != nil {
		g.uplink.Close()
	}
	if g.downlink != nil {
		g.downlink.Close()
	}
	if g.status != nil {
		g.status.Close()
	}
	g.client.Close()
}

const maxBackOff = 5 * time.Minute

// Backend implements the TTN backend
type Backend struct {
	routerConn    *grpc.ClientConn
	routerClient  pb_router.RouterClient
	rxRateLimit   float64
	txPacketChan  chan gw.TXPacketBytes
	gatewayConf   map[lorawan.EUI64]*gtwConf
	gateways      map[lorawan.EUI64]*gtw
	gatewayStatus *pb_gateway.Status
	mutex         sync.RWMutex
}

func (b *Backend) getGtw(mac lorawan.EUI64) *gtw {
	defer b.mutex.RUnlock()
	b.mutex.RLock()
	if gtw, ok := b.gateways[mac]; ok {
		return gtw
	}
	return nil
}

func (b *Backend) newGtw(mac lorawan.EUI64) *gtw {
	defer b.mutex.Unlock()
	b.mutex.Lock()
	if _, ok := b.gateways[mac]; !ok {
		gatewayID := fmt.Sprintf("eui-%s", mac)
		gatewayToken := "token"
		if conf, ok := b.gatewayConf[mac]; ok {
			gatewayID = conf.id
			gatewayToken = conf.token
		}
		gateway := &gtw{
			client: pb_router.NewRouterClientForGateway(b.routerClient, gatewayID, gatewayToken),
			rxRate: metrics.NewEWMA1(),
		}
		gateway.uplink = pb_router.NewMonitoredUplinkStream(gateway.client)
		gateway.status = pb_router.NewMonitoredGatewayStatusStream(gateway.client)
		b.gateways[mac] = gateway
	}
	return b.gateways[mac]
}

// NewBackend creates a new Backend.
func NewBackend(discovery, router string) (b *Backend, err error) {
	b = &Backend{
		txPacketChan:  make(chan gw.TXPacketBytes),
		gatewayConf:   make(map[lorawan.EUI64]*gtwConf),
		gateways:      make(map[lorawan.EUI64]*gtw),
		gatewayStatus: new(pb_gateway.Status),
	}

	var announcement pb_discovery.Announcement
	if discovery == "" {
		announcement.NetAddress = router
	} else {
		log.Info("backend/thethingsnetwork: connecting to discovery server...")
		discovery, err := pb_discovery.NewClient(discovery, &pb_discovery.Announcement{
			ServiceName: "lora-gateway-bridge",
			Id:          getID(),
		}, func() string { return "" })
		if err != nil {
			return nil, err
		}
		defer discovery.Close()

		log.Info("backend/thethingsnetwork: getting router from discovery server")
		router, err := discovery.Get("router", router)
		if err != nil {
			return nil, err
		}

		announcement = *router
	}

	log.Info("backend/thethingsnetwork: connecting to router...")
	b.routerConn, err = announcement.Dial()
	if err != nil {
		return nil, err
	}
	b.routerClient = pb_router.NewRouterClient(b.routerConn)

	log.Info("backend/thethingsnetwork: connected to router")

	// Tick gateway rates
	go func() {
		for {
			b.tick()
			time.Sleep(5 * time.Second)
		}
	}()

	return b, nil
}

// SetRxRateLimit limits the rate at which gateways can send Rx (per minute).
func (b *Backend) SetRxRateLimit(limit float64) {
	b.rxRateLimit = limit
}

// AddGateway adds the configuration of a gateway
func (b *Backend) AddGateway(euiStr, id, token string) error {
	var eui lorawan.EUI64
	if err := eui.UnmarshalText([]byte(euiStr)); err != nil {
		return err
	}
	b.gatewayConf[eui] = &gtwConf{
		id:    id,
		token: token,
	}
	return nil
}

// InjectRegion injects a region string into each gateway status
func (b *Backend) InjectRegion(region string) {
	b.gatewayStatus.Region = region
}

// InjectRTT injects a RTT into each gateway status
func (b *Backend) InjectRTT(rtt uint) {
	b.gatewayStatus.Rtt = uint32(rtt)
}

func (b *Backend) tick() {
	defer b.mutex.RUnlock()
	b.mutex.RLock()
	for _, gtw := range b.gateways {
		gtw.rxRate.Tick()
	}
}

// Close closes the backend.
func (b *Backend) Close() {
	defer b.mutex.Unlock()
	b.mutex.Lock()
	for _, gtw := range b.gateways {
		gtw.Close()
	}
	b.routerConn.Close()
}

// TXPacketChan returns the TXPacketBytes channel.
func (b *Backend) TXPacketChan() chan gw.TXPacketBytes {
	return b.txPacketChan
}

// SubscribeGatewayTX subscribes the backend to the gateway TXPacket
// topic (packets the gateway needs to transmit).
func (b *Backend) SubscribeGatewayTX(mac lorawan.EUI64) error {
	log := log.WithField("gateway", mac)

	gtw := b.getGtw(mac)
	if gtw == nil {
		gtw = b.newGtw(mac)
	}

	gtw.downlink = pb_router.NewMonitoredDownlinkStream(gtw.client)

	go func() {
		for in := range gtw.downlink.Channel() {
			log.Info("backend/thethingsnetwork: message received")
			lora := in.ProtocolConfiguration.GetLorawan()
			if lora == nil {
				log.Error("backend/thethingsnetwork: received non-Lora message")
				continue
			}

			var dataRate band.DataRate

			if lora.Modulation == pb_lorawan.Modulation_LORA {
				dr, _ := types.ParseDataRate(lora.DataRate)
				dataRate.Modulation = band.LoRaModulation
				dataRate.SpreadFactor = int(dr.SpreadingFactor)
				dataRate.Bandwidth = int(dr.Bandwidth)
			}

			if lora.Modulation == pb_lorawan.Modulation_FSK {
				dataRate.Modulation = band.FSKModulation
				dataRate.BitRate = int(lora.BitRate)
			}

			var txPacket gw.TXPacketBytes
			txPacket.TXInfo = gw.TXInfo{
				MAC:       mac,
				Timestamp: in.GatewayConfiguration.Timestamp,
				Frequency: int(in.GatewayConfiguration.Frequency),
				Power:     int(in.GatewayConfiguration.Power),
				DataRate:  dataRate,
				CodeRate:  lora.CodingRate,
			}
			txPacket.PHYPayload = in.Payload
			b.txPacketChan <- txPacket
		}
	}()

	return nil
}

// UnSubscribeGatewayTX unsubscribes the backend from the gateway TXPacket
// topic.
func (b *Backend) UnSubscribeGatewayTX(mac lorawan.EUI64) error {
	gtw := b.getGtw(mac)
	if gtw == nil || gtw.downlink == nil {
		return nil
	}
	gtw.downlink.Close()
	return nil
}

func (b *Backend) convertRXPacket(rxPacket gw.RXPacketBytes) *pb_router.UplinkMessage {
	// Convert some Modulation-dependent fields
	var modulation pb_lorawan.Modulation
	var datarate string
	var bitrate uint32
	switch rxPacket.RXInfo.DataRate.Modulation {
	case band.LoRaModulation:
		modulation = pb_lorawan.Modulation_LORA
		datarate = fmt.Sprintf("SF%dBW%d", rxPacket.RXInfo.DataRate.SpreadFactor, rxPacket.RXInfo.DataRate.Bandwidth)
	case band.FSKModulation:
		modulation = pb_lorawan.Modulation_FSK
		bitrate = uint32(rxPacket.RXInfo.DataRate.BitRate)
	}

	return &pb_router.UplinkMessage{
		Payload: rxPacket.PHYPayload,
		ProtocolMetadata: &pb_protocol.RxMetadata{Protocol: &pb_protocol.RxMetadata_Lorawan{Lorawan: &pb_lorawan.Metadata{
			Modulation: modulation,
			DataRate:   datarate,
			BitRate:    bitrate,
			CodingRate: rxPacket.RXInfo.CodeRate,
		}}},
		GatewayMetadata: &pb_gateway.RxMetadata{
			GatewayId: fmt.Sprintf("eui-%s", rxPacket.RXInfo.MAC),
			Timestamp: rxPacket.RXInfo.Timestamp,
			Time:      rxPacket.RXInfo.Time.UnixNano(),
			RfChain:   uint32(rxPacket.RXInfo.RFChain),
			Channel:   uint32(rxPacket.RXInfo.Channel),
			Frequency: uint64(rxPacket.RXInfo.Frequency),
			Rssi:      float32(rxPacket.RXInfo.RSSI),
			Snr:       float32(rxPacket.RXInfo.LoRaSNR),
		},
	}
}

// PublishGatewayRX publishes a RX packet to the MQTT broker.
func (b *Backend) PublishGatewayRX(mac lorawan.EUI64, rxPacket gw.RXPacketBytes) error {
	gtw := b.getGtw(mac)
	if gtw == nil {
		gtw = b.newGtw(mac)
	}
	gtw.rxRate.Update(1)
	if b.rxRateLimit > 0 && gtw.rxRate.Rate() > b.rxRateLimit {
		return nil
	}
	return gtw.uplink.Send(b.convertRXPacket(rxPacket))
}

func (b *Backend) convertStatsPacket(stats gw.GatewayStatsPacket) *pb_gateway.Status {
	status := *b.gatewayStatus // Copy from the defaults

	status.Time = stats.Time.UnixNano()
	status.RxIn = uint32(stats.RXPacketsReceived)
	status.RxOk = uint32(stats.RXPacketsReceivedOK)

	if platform, ok := stats.CustomData["platform"]; ok {
		if platform, ok := platform.(string); ok {
			status.Platform = string(platform)
		}
	}
	if contactEmail, ok := stats.CustomData["contactEmail"]; ok {
		if contactEmail, ok := contactEmail.(string); ok {
			status.ContactEmail = string(contactEmail)
		}
	}
	if description, ok := stats.CustomData["description"]; ok {
		if description, ok := description.(string); ok {
			status.Description = string(description)
		}
	}
	if ip, ok := stats.CustomData["ip"]; ok {
		if ip, ok := ip.([]string); ok {
			status.Ip = ip
		}
	}
	if stats.Latitude != 0 || stats.Longitude != 0 || stats.Altitude != 0 {
		status.Gps = &pb_gateway.GPSMetadata{
			Latitude:  float32(stats.Latitude),
			Longitude: float32(stats.Longitude),
			Altitude:  int32(stats.Altitude),
		}
	}

	return &status
}

// PublishGatewayStats publishes a GatewayStatsPacket to the MQTT broker.
func (b *Backend) PublishGatewayStats(mac lorawan.EUI64, stats gw.GatewayStatsPacket) error {
	gtw := b.getGtw(mac)
	if gtw == nil {
		gtw = b.newGtw(mac)
	}
	return gtw.status.Send(b.convertStatsPacket(stats))
}
