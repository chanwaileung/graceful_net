package main

import (
	"context"
	"google.golang.org/grpc"
	pb "graceful_net/proto"
	"log"
	"time"
)

func init() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
}

func main() {
	conn, err := grpc.Dial("192.168.204.204:1112", grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connection:%s", err.Error())
	}
	defer conn.Close()

	cli := pb.NewServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second * 30)
	defer cancel()

	r, err := cli.Put(ctx, &pb.GetRequest{
		Carno:   "粤A123456",
		Cartype: "",
		Vcode:   "",
	})
	if err != nil {
		log.Fatalf("Put request error:%s", err.Error())
	}

	log.Printf("Put response:%s\n", r)
}
