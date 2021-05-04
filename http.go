package main

import (
	"encoding/base64"
	"encoding/json"
	"golang.org/x/net/websocket"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/deepch/vdk/av"
	"github.com/gin-gonic/gin"

	mse "github.com/deepch/vdk/format/mp4f"
	webrtc "github.com/deepch/vdk/format/webrtcv3"
)

type JCodec struct {
	Type string
}

func serveHTTP() {
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	router.Use(CORSMiddleware())

	if _, err := os.Stat("./web"); !os.IsNotExist(err) {
		router.LoadHTMLGlob("web/templates/*")
		router.GET("/", HTTPAPIServerIndex)
		router.GET("/stream/player/:uuid", HTTPAPIServerStreamPlayer)
	}
	router.POST("/stream/receiver/:uuid", HTTPAPIServerStreamWebRTC)
	router.GET("/stream/codec/:uuid", HTTPAPIServerStreamCodec)

	router.GET("/ws", func(c *gin.Context) {
		handler := websocket.Handler(ws)
		s := websocket.Server{Handler: handler}
		s.ServeHTTP(c.Writer, c.Request)
	})

	router.StaticFS("/static", http.Dir("web/static"))
	err := router.Run(Config.Server.HTTPPort)
	if err != nil {
		log.Fatalln("Start HTTP Server error", err)
	}
}

//HTTPAPIServerIndex  index
func HTTPAPIServerIndex(c *gin.Context) {
	_, all := Config.list()
	if len(all) > 0 {
		c.Header("Cache-Control", "no-cache, max-age=0, must-revalidate, no-store")
		c.Header("Access-Control-Allow-Origin", "*")
		c.Redirect(http.StatusMovedPermanently, "stream/player/"+all[0])
	} else {
		c.HTML(http.StatusOK, "index.tmpl", gin.H{
			"port":    Config.Server.HTTPPort,
			"version": time.Now().String(),
		})
	}
}

//HTTPAPIServerStreamPlayer stream player
func HTTPAPIServerStreamPlayer(c *gin.Context) {
	_, all := Config.list()
	sort.Strings(all)
	c.HTML(http.StatusOK, "player.tmpl", gin.H{
		"port":     Config.Server.HTTPPort,
		"suuid":    c.Param("uuid"),
		"suuidMap": all,
		"version":  time.Now().String(),
	})
}

//HTTPAPIServerStreamCodec stream codec
func HTTPAPIServerStreamCodec(c *gin.Context) {
	if Config.ext(c.Param("uuid")) {
		Config.RunIFNotRun(c.Param("uuid"))
		codecs := Config.coGe(c.Param("uuid"))
		if codecs == nil {
			return
		}
		var tmpCodec []JCodec
		for _, codec := range codecs {
			if codec.Type() != av.H264 && codec.Type() != av.PCM_ALAW && codec.Type() != av.PCM_MULAW && codec.Type() != av.OPUS {
				log.Println("Codec Not Supported WebRTC ignore this track", codec.Type())
				continue
			}
			if codec.Type().IsVideo() {
				tmpCodec = append(tmpCodec, JCodec{Type: "video"})
			} else {
				tmpCodec = append(tmpCodec, JCodec{Type: "audio"})
			}
		}
		b, err := json.Marshal(tmpCodec)
		if err == nil {
			_, err = c.Writer.Write(b)
			if err != nil {
				log.Println("Write Codec Info error", err)
				return
			}
		}
	}
}

//HTTPAPIServerStreamWebRTC stream video over WebRTC
func HTTPAPIServerStreamWebRTC(c *gin.Context) {
	if !Config.ext(c.PostForm("suuid")) {
		log.Println("Stream Not Found")
		return
	}
	Config.RunIFNotRun(c.PostForm("suuid"))
	codecs := Config.coGe(c.PostForm("suuid"))
	if codecs == nil {
		log.Println("Stream Codec Not Found")
		return
	}
	var AudioOnly bool
	if len(codecs) == 1 && codecs[0].Type().IsAudio() {
		AudioOnly = true
	}
	muxerWebRTC := webrtc.NewMuxer(webrtc.Options{ICEServers: Config.GetICEServers(), ICEUsername: Config.GetICEUsername(), ICECredential: Config.GetICECredential(), PortMin: Config.GetWebRTCPortMin(), PortMax: Config.GetWebRTCPortMax()})
	answer, err := muxerWebRTC.WriteHeader(codecs, c.PostForm("data"))
	if err != nil {
		log.Println("WriteHeader", err)
		return
	}
	_, err = c.Writer.Write([]byte(answer))
	if err != nil {
		log.Println("Write", err)
		return
	}
	go func() {
		cid, ch := Config.clAd(c.PostForm("suuid"))
		defer Config.clDe(c.PostForm("suuid"), cid)
		defer muxerWebRTC.Close()
		var videoStart bool
		noVideo := time.NewTimer(10 * time.Second)
		for {
			select {
			case <-noVideo.C:
				log.Println("noVideo")
				return
			case pck := <-ch:
				if pck.IsKeyFrame || AudioOnly {
					noVideo.Reset(10 * time.Second)
					videoStart = true
				}
				if !videoStart && !AudioOnly {
					continue
				}
				err = muxerWebRTC.WritePacket(pck)
				if err != nil {
					log.Println("WritePacket", err)
					return
				}
			}
		}
	}()
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization, x-access-token")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers, Cache-Control, Content-Language, Content-Type")
		c.Header("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

type Request struct {
	Type string `json:"type"`
	Sdp  string `json:"sdp,omitempty"`
}

type Response struct {
	Type   string `json:"type,omitempty"`
	Codecs string `json:"codecs,omitempty"`
	Error  string `json:"error,omitempty"`
	Sdp    string `json:"sdp,omitempty"`
}

func ws(ws *websocket.Conn) {
	defer ws.Close()

	url := ws.Request().URL.Query().Get("url")
	if _, ok := Config.Streams[url]; !ok {
		Config.Streams[url] = StreamST{
			URL:      url,
			OnDemand: true,
			Cl:       make(map[string]viewer),
		}
	}

	Config.RunIFNotRun(url)

	err := ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err != nil {
		log.Println("ws.SetWriteDeadline", err)
		err = websocket.JSON.Send(ws, Response{Error: err.Error()})
		if err != nil {
			log.Println("websocket.JSON.Send", err)
		}
		return
	}

	codecs := Config.coGe(url)
	if codecs == nil {
		log.Println("Stream Codec Not Found")
		err = websocket.JSON.Send(ws, Response{Error: Config.LastError.Error()})
		if err != nil {
			log.Println("websocket.JSON.Send", err)
		}
		return
	}

	for {
		var request Request
		err := websocket.JSON.Receive(ws, &request)
		if err != nil {
			log.Println("websocket.JSON.Receive", err)
			return
		}
		switch request.Type {
		case "mse":
			go startMSE(ws, url)
		case "webrtc":
			go startWebRTC(ws, url, request.Sdp)
		}
	}
}

func startMSE(ws *websocket.Conn, url string) {
	codecs := Config.coGe(url)

	mseCodecs := make([]av.CodecData, 0)
	mseIdx := make(map[int8]bool)
	for i, codec := range codecs {
		switch codec.Type() {
		case av.H264, av.H265, av.AAC:
			mseCodecs = append(mseCodecs, codec)
			mseIdx[int8(i)] = true
		}
	}

	mseMuxer := mse.NewMuxer(nil)
	// only supported codecs
	err := mseMuxer.WriteHeader(mseCodecs)
	if err != nil {
		log.Println("mseMuxer.WriteHeader", err)
		err = websocket.JSON.Send(ws, Response{Error: err.Error()})
		if err != nil {
			log.Println("websocket.JSON.Send", err)
		}
		return
	}

	meta, init := mseMuxer.GetInit(mseCodecs)
	err = websocket.JSON.Send(ws, Response{Type: "mse", Codecs: meta})
	if err != nil {
		log.Println("websocket.JSON.Send", err)
		return
	}

	err = websocket.Message.Send(ws, init)
	if err != nil {
		log.Println("websocket.Message.Send", err)
		return
	}

	cid, ch := Config.clAd(url)
	defer Config.clDe(url, cid)

	noVideo := time.NewTimer(10 * time.Second)
	var start bool
	for {
		select {
		case <-noVideo.C:
			log.Println("noVideo")
			err = websocket.JSON.Send(ws, Response{Error: "No video"})
			if err != nil {
				log.Println("websocket.JSON.Send", err)
			}
			return
		case pck := <-ch:
			if pck.IsKeyFrame {
				noVideo.Reset(10 * time.Second)
				start = true
			}
			if _, ok := mseIdx[pck.Idx]; !start || !ok {
				continue
			}

			ready, buf, _ := mseMuxer.WritePacket(pck, false)
			if ready {
				err = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err != nil {
					return
				}

				err = websocket.Message.Send(ws, buf)
				if err != nil {
					log.Println("websocket.Message.Send", err)
					return
				}
			}
		}
	}
}

func startWebRTC(ws *websocket.Conn, url string, sdp string) {
	muxerWebRTC := webrtc.NewMuxer(
		webrtc.Options{
			ICEServers: Config.GetICEServers(),
			PortMin:    Config.GetWebRTCPortMin(),
			PortMax:    Config.GetWebRTCPortMax(),
		},
	)

	codecs := Config.coGe(url)

	sdp64 := base64.StdEncoding.EncodeToString([]byte(sdp))
	// check unsupported codecs inside
	sdp64, err := muxerWebRTC.WriteHeader(codecs, sdp64)
	if err != nil {
		log.Println("muxerWebRTC.WriteHeader", err)
		return
	}
	sdpB, _ := base64.StdEncoding.DecodeString(sdp64)

	err = websocket.JSON.Send(ws, Response{Type: "webrtc", Sdp: string(sdpB)})
	if err != nil {
		log.Println("websocket.JSON.Send", err)
		return
	}

	// TODO: try to use single cid/ch
	cid, ch := Config.clAd(url)
	defer Config.clDe(url, cid)

	defer muxerWebRTC.Close()

	var start bool
	noVideo := time.NewTimer(10 * time.Second)
	for {
		select {
		case <-noVideo.C:
			log.Println("noVideo")
			return
		case pck := <-ch:
			if pck.IsKeyFrame {
				noVideo.Reset(10 * time.Second)
				start = true
			}
			if !start {
				continue
			}
			// check unsupported codecs inside
			err = muxerWebRTC.WritePacket(pck)
			if err != nil {
				log.Println("muxerWebRTC.WritePacket", err)
				return
			}
		}
	}
}
