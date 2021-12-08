package main

import (
	"context"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	pb "graceful_net/proto"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

type myHandler struct{}

//定义一个map来实现路由转发
var (
	httpServer   http.Server
	grpcServer   *grpc.Server
	httpListener net.Listener
	grpcListener net.Listener
	graceful     = flag.Bool("graceful", false, "listen on fd open 3 (internal use only)")
	mux          map[string]func(http.ResponseWriter, *http.Request)
)

func init() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {

	var err error
	if *graceful {
		log.Printf("热更新启动")
		//为什么是3呢，而不是1 0 或者其他数字？这是因为父进程里给了个fd给子进程了 而子进程里0，1，2是预留给 标准输入、输出和错误的，所以父进程给的第一个fd在子进程里顺序排就是从3开始了；如果fork的时候cmd.ExtraFiles给了两个文件句柄，那么子进程里还可以用4开始，就看你开了几个子进程自增就行。因为我这里就开一个子进程所以把3写死了。l, err = net.FileListener(f)这一步只是把 fd描述符包装进TCPListener这个结构体。
		f3 := os.NewFile(3, "")
		//先复制fd到新的fd, 然后设置子进程exec时自动关闭父进程的fd,即“F_DUPFD_CLOEXEC”
		httpListener, err = net.FileListener(f3)
		if err != nil {
			log.Fatalf("f3 to listener error: %v", err)
		}

		f4 := os.NewFile(4, "")
		grpcListener, err = net.FileListener(f4)
		if err != nil {
			log.Fatalf("f4 to listener error: %v", err)
		}
	} else {
		log.Printf("非热更新启动")
		httpListener, err = net.Listen("tcp", ":1111")
		if err != nil {
			log.Fatalf("http listener error: %v", err)
		}

		grpcListener, err = net.Listen("tcp", ":1112")
		if err != nil {
			log.Fatalf("grpc listener error: %v", err)
		}
	}

	//分别启动http服务和grpc服务
	go beginGrpc()
	go beginHttp()
	listenSignal()
	log.Println("process end")
}

//监听信号量
func listenSignal() {
	var sigList []os.Signal

	//暂时使用-USR2信号量进行热更新,所以暂时只有linux支持热更新
	if runtime.GOOS == "linux" {
		//因为编译问题无法直接使用syscall.SIGUSR1，故只能使用Signal方法避过不同平台的编译差异性
		//syscall.SIGUSR1 == syscall.Signal(10) == SIGUSR1
		sigList = append(sigList, syscall.SIGINT, syscall.SIGTERM, syscall.Signal(10))
	} else {
		sigList = append(sigList, syscall.SIGINT, syscall.SIGTERM)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, sigList...)

	//监控信号量
	//for {		//如果使用for循环监听的话，会导致有两个进程同时存在，因为第一个信号量给捕获的时候，ch给stop了，但是for还在继续，会导致进程阻塞了，然后存在两个进程（一个是在服务的，一个是被for阻塞的空进程），直到下次传入kill时候，才会结束旧进程，因为ch被stop了，所以kill直接结束了旧进程。但此时还是会有两个进程的。
		log.Println("wait signal")
		sig := <-c
		log.Printf("signal: %v", sig)
		//设置父进程的结束时间，现有的请求在时间内相应或者超时处理
		ctx, _ := context.WithTimeout(context.Background(), 20*time.Second)
		switch sig {
		case syscall.Signal(10):
			signal.Stop(c)
			log.Println("开始构建cmd")
			if err := reload(); err != nil {
				log.Fatalf("热更新构建失败: %v", err)
			}
			httpServer.Shutdown(ctx)
			grpcServer.GracefulStop()

			//用这种方式也不会有问题,所以可以用这种方式在热更新grpc的时候,结束旧进程
			//2021-12-08	直接使用exit的话，会导致正在执行的请求都给截至了，不可行
			//os.Exit(1)
			log.Println("热更新完成")
		case syscall.SIGINT, syscall.SIGTERM:
			//优雅退出
			signal.Stop(c)
			log.Println("stop")
			httpServer.Shutdown(ctx)
			grpcServer.Stop()
			log.Print("graceful shutdown")
		}
	//}
	log.Println("signal end")
}

//热更新 -- 平滑重启
func reload() error {
	//获取当前进程的FD
	t3, ok := httpListener.(*net.TCPListener)
	if !ok {
		panic("listener is not tcp listener")
	}
	FD3, err := t3.File()
	if err != nil {
		return err
	}
	t4, ok := grpcListener.(*net.TCPListener)
	if !ok {
		panic("listener is not tcp listener")
	}
	FD4, err := t4.File()
	if err != nil {
		return err
	}

	//args := []string{"-graceful"}
	var args []string
	//如果用-graceful方式启动,直接追加-graceful参数的话,会导致重复参数,所以先排除该该参数
	for _, v := range os.Args {
		if v != "-graceful" {
			args = append(args, v)
		}
	}
	args = append(args, "-graceful") //增加参数，平滑重启
	cmd := exec.Command(os.Args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	//cmd.ExtraFiles = socketFile		//父进程套接字
	cmd.ExtraFiles = []*os.File{FD3, FD4}		//按顺序传入FD文件，然后再按顺序取出，转成listener
	cmd.Dir, _ = os.Getwd()

	return cmd.Start()
}

//------------http设置------------
func beginHttp() {
	var err error

	//定义路由
	mux = make(map[string]func(http.ResponseWriter, *http.Request))
	mux["/"] = index

	//定义server
	httpServer = http.Server{
		Handler:     &myHandler{},
		ReadTimeout: 15 * time.Second,
		//Addr: ":1111",		//监听的端口也可以在这里声明
	}

	//将listener绑定到server上，并运行
	err = httpServer.Serve(httpListener)
	if err != nil {
		log.Println(err.Error())		//注意：这里 一定一定一定 不能用Fatal，不然会导致进程直接结束，请求得到EOF的空返回
	}
}

func (*myHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handleFunc, ok := mux[r.URL.String()]; ok {
		handleFunc(w, r)
		return
	}
	//第一种回写方式
	io.WriteString(w, "error cwl")
}

func index(w http.ResponseWriter, r *http.Request) {
	//第二种回写方式
	time.Sleep(10 * time.Second) //todo 这个时间根据需求调整
	fmt.Fprintf(w, "hello cwl\n")
}

//------------http设置------------



//------------grpc设置------------
type server struct{}

func (s *server) Get(ctx context.Context,req *pb.GetRequest) (*pb.ServiceResponse, error) {
	return &pb.ServiceResponse{
		Success: true,
		Code:    1,
		Data:    "get option success：" + req.Carno,
		Err:     "",
	}, nil
}

func (s *server) Put(ctx context.Context, req *pb.GetRequest) (*pb.ServiceResponse, error) {
	time.Sleep(10 * time.Second)
	return &pb.ServiceResponse{
		Success: true,
		Code:    1,
		Data:    "put option success：" + req.Carno,
		Err:     "",
	}, nil
}

func beginGrpc() {
	grpcServer = grpc.NewServer()
	pb.RegisterServiceServer(grpcServer, &server{})
	reflection.Register(grpcServer)
	if err := grpcServer.Serve(grpcListener); err != nil {
		log.Printf("Grpc is error:%s\n", err.Error())		//注意：这里 一定一定一定 不能用Fatal，不然会导致进程直接结束，请求得到EOF的空返回
	}
}
//------------grpc设置------------
