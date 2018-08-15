package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/influxdb/client/v2"
)

//读取接口
type Reader interface {
	Read(rc chan []byte)
}

//写入接口
type Writer interface {
	Write(wc chan *Message)
}

type LogProcess struct {
	rc    chan []byte   //负责读取模块到解析模块的数据传输
	wc    chan *Message //负责解析模块到写入模块的数据传输
	read  Reader        //读取模块
	write Writer        //写入模块
}

type ReadFromFile struct {
	path string // 读取文件的路径
}

type WriteToInfluxDB struct {
	influxDBDsn string // influx data source
}

//存储提取出来的监控数据
type Message struct {
	TimeLocal                    time.Time
	BytesSent                    int
	Path, Method, Scheme, Status string
	UpstreamTime, RequestTime    float64
}

// 系统状态监控
type SystemInfo struct {
	HandleLine   int     `json:"handleLine"`   // 总处理日志行数
	Tps          float64 `json:"tps"`          // 系统吞出量
	ReadChanLen  int     `json:"readChanLen"`  // read channel 长度
	WriteChanLen int     `json:"writeChanLen"` // write channel 长度
	RunTime      string  `json:"runTime"`      // 运行总时间
	ErrNum       int     `json:"errNum"`       // 错误数
}

const (
	TypeHandleLine = 0
	TypeErrNum     = 1
)

//用于统计行数和错误数的数据传输channel
var TypeMonitorChan = make(chan int, 200)

//监控模块的封装
type Monitor struct {
	startTime time.Time
	data      SystemInfo
	tpsSli    []int
}

//开始监控
func (m *Monitor) start(lp *LogProcess) {

	go func() {
		//统计行数和错误数
		for n := range TypeMonitorChan {
			switch n {
			case TypeErrNum:
				m.data.ErrNum += 1
			case TypeHandleLine:
				m.data.HandleLine += 1
			}
		}
	}()

	//每5s统计一次tps
	ticker := time.NewTicker(time.Second * 5)
	go func() {
		for {
			<-ticker.C
			m.tpsSli = append(m.tpsSli, m.data.HandleLine)
			//保持最新的两个总的日志行数就可以了
			if len(m.tpsSli) > 2 {
				m.tpsSli = m.tpsSli[1:]
			}
		}
	}()

	http.HandleFunc("/monitor", func(writer http.ResponseWriter, request *http.Request) {
		m.data.RunTime = time.Now().Sub(m.startTime).String() //计算当前程序的运行时间
		m.data.ReadChanLen = len(lp.rc)
		m.data.WriteChanLen = len(lp.wc)

		//每5s统计一次tps, tps=(总日志数2-总日志数1)/5
		if len(m.tpsSli) >= 2 {
			m.data.Tps = float64(m.tpsSli[1]-m.tpsSli[0]) / 5
		}

		//渲染
		ret, _ := json.MarshalIndent(m.data, "", "\t") //转出来的json打印出来比较好看
		io.WriteString(writer, string(ret))
	})

	http.ListenAndServe(":9193", nil)
}

// 读取模块:读取日志文件后交给解析模块
func (r *ReadFromFile) Read(rc chan []byte) {

	// 打开文件
	f, err := os.Open(r.path)
	if err != nil {
		panic(fmt.Sprintf("open file error:%s", err.Error()))
	}

	// 从文件末尾开始逐行读取文件内容
	// 因为既然是监控,我们想要读取的是最新的数据,所有选择从末尾读取
	f.Seek(0, 2)             //把文件指针移动到文件末尾
	rd := bufio.NewReader(f) //因为f只能一个一个字节的读,因此先用bufio转成一个新的rd,支持更多读取操作

	//循环逐行读取文件
	for {
		line, err := rd.ReadBytes('\n') //读取内容直到遇到\n,即读取一行内容
		//EOF表示读取到文件末尾,则sleep,等有新的信息产生
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		} else if err != nil {
			panic(fmt.Sprintf("ReadBytes error:%s", err.Error()))
		}
		TypeMonitorChan <- TypeHandleLine //读取到一行,则添加一个标记,用于后续统计读取个数
		rc <- line[:len(line)-1]          //这里是表示不要最后的换行符
	}
}

// 写入模块:把从解析模块得到的数据交给influxdb存储
func (w *WriteToInfluxDB) Write(wc chan *Message) {

	infSli := strings.Split(w.influxDBDsn, "@")

	// Create a new influxdb HTTPClient
	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     infSli[0],
		Username: infSli[1],
		Password: infSli[2],
	})
	if err != nil {
		log.Fatal(err)
	}

	for v := range wc {
		// Create a new point batch
		bp, err := client.NewBatchPoints(client.BatchPointsConfig{
			Database:  infSli[3],
			Precision: infSli[4], //精度,秒s
		})
		if err != nil {
			log.Fatal(err)
		}

		// Create a point and add to batch
		// Tags: Path, Method, Scheme, Status
		tags := map[string]string{"Path": v.Path, "Method": v.Method, "Scheme": v.Scheme, "Status": v.Status}
		// Fields: UpstreamTime, RequestTime, BytesSent
		fields := map[string]interface{}{
			"UpstreamTime": v.UpstreamTime,
			"RequestTime":  v.RequestTime,
			"BytesSent":    v.BytesSent,
		}

		pt, err := client.NewPoint("nginx_log", tags, fields, v.TimeLocal)
		if err != nil {
			log.Fatal(err)
		}
		bp.AddPoint(pt)

		// Write the batch
		if err := c.Write(bp); err != nil {
			log.Fatal(err)
		}

		log.Println("write success!")
	}
}

// 解析模块:正则解析读取模块传递的数据,并交给写入模块
func (l *LogProcess) Process() {

	/**
	172.0.0.12 - - [04/Mar/2018:13:49:52 +0000] http "GET /foo?query=t HTTP/1.0" 200 2133 "-" "KeepAliveClient" "-" 1.005 1.854
	*/

	r := regexp.MustCompile(`([\d\.]+)\s+([^ \[]+)\s+([^ \[]+)\s+\[([^\]]+)\]\s+([a-z]+)\s+\"([^"]+)\"\s+(\d{3})\s+(\d+)\s+\"([^"]+)\"\s+\"(.*?)\"\s+\"([\d\.-]+)\"\s+([\d\.-]+)\s+([\d\.-]+)`)

	loc, _ := time.LoadLocation("Asia/Shanghai") //上海时区
	for v := range l.rc {
		ret := r.FindStringSubmatch(string(v))
		if len(ret) != 14 {
			TypeMonitorChan <- TypeErrNum //读取到错误,则添加标记
			log.Println("FindStringSubmatch fail:", string(v))
			continue
		}

		message := &Message{}

		//解析出时间
		t, err := time.ParseInLocation("02/Jan/2006:15:04:05 +0000", ret[4], loc)
		if err != nil {
			TypeMonitorChan <- TypeErrNum //读取到错误,则添加标记
			log.Println("ParseInLocation fail:", err.Error(), ret[4])
			continue
		}
		message.TimeLocal = t

		byteSent, _ := strconv.Atoi(ret[8])
		message.BytesSent = byteSent

		// GET /foo?query=t HTTP/1.0
		reqSli := strings.Split(ret[6], " ") //按空格分割
		if len(reqSli) != 3 {
			TypeMonitorChan <- TypeErrNum //读取到错误,则添加标记
			log.Println("strings.Split fail", ret[6])
			continue
		}
		message.Method = reqSli[0]

		u, err := url.Parse(reqSli[1])
		if err != nil {
			log.Println("url parse fail:", err)
			TypeMonitorChan <- TypeErrNum //读取到错误,则添加标记
			continue
		}
		message.Path = u.Path

		message.Scheme = ret[5]
		message.Status = ret[7]

		upstreamTime, _ := strconv.ParseFloat(ret[12], 64)
		requestTime, _ := strconv.ParseFloat(ret[13], 64)
		message.UpstreamTime = upstreamTime
		message.RequestTime = requestTime

		l.wc <- message
	}
}

func main() {
	var path, influxDsn string
	//用于解析参数
	flag.StringVar(&path, "path", "./access.log", "read file path")
	flag.StringVar(&influxDsn, "influxDsn", "http://127.0.0.1:8086@imooc@imoocpass@imooc@s", "influx data source")
	flag.Parse()

	r := &ReadFromFile{
		path: path,
	}

	w := &WriteToInfluxDB{
		influxDBDsn: influxDsn,
	}

	lp := &LogProcess{
		rc:    make(chan []byte, 200),
		wc:    make(chan *Message, 200),
		read:  r,
		write: w,
	}

	go lp.read.Read(lp.rc)
	for i := 0; i < 2; i++ {
		go lp.Process()
	}

	for i := 0; i < 4; i++ {
		go lp.write.Write(lp.wc)
	}

	m := &Monitor{
		startTime: time.Now(),
		data:      SystemInfo{},
	}
	m.start(lp)
}
