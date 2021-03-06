package gateway

import (
	"context"
	"fmt"
	"net"
	"sync"

	"golang.org/x/xerrors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/mrasu/Cowloon/pkg/lib"

	"github.com/mrasu/Cowloon/pkg/migrator"

	"errors"

	_ "github.com/go-sql-driver/mysql"
	"github.com/mrasu/Cowloon/pkg/db"
	"github.com/mrasu/Cowloon/pkg/protos"
)

const (
	ErrorNum = -2147483648
	port     = ":15501"
)

type Manager struct {
	router *Router

	runningQueryWg *sync.WaitGroup
	stopper        *lib.Stopper
}

func NewManager() (*Manager, error) {
	r, err := NewRouter()
	if err != nil {
		return nil, err
	}

	return &Manager{
		router:         r,
		runningQueryWg: new(sync.WaitGroup),
		stopper:        lib.NewStopper(),
	}, nil
}

func (m *Manager) Start() {
	ctx, canFn := lib.WithPanic(context.Background())
	go m.router.StartHeatBeat(ctx)
	go m.startGrpcServer(ctx)

	err := <-ctx.Panicked()
	fmt.Printf("Error: %+v\n", err)
	fmt.Println("Stopping....")

	finWg := canFn()
	finWg.Wait()

	panic(err)
}

func (m *Manager) startGrpcServer(parent *lib.PanickerContext) {
	ctx, canFn := lib.WithPanic(parent)
	defer ctx.Finish()

	lis, err := net.Listen("tcp", port)
	if err != nil {
		ctx.Panic(xerrors.Errorf("failed to start grpc: %v", err))
		return
	}

	gs := grpc.NewServer()
	protos.RegisterUserMessageServer(gs, m)
	reflection.Register(gs)

	errCh := make(chan error)
	go func() {
		if err = gs.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	select {
	case err = <-errCh:
		canFn()
		ctx.Panic(xerrors.Errorf("failed to start grpc: %v", err))
	case <-ctx.Done():
		gs.Stop()
		ctx.Panic(xerrors.Errorf("grpc terminated: %v", ctx.Err()))
	case <-parent.Done():
		fmt.Println("terminating grpc...")
		gs.Stop()
	}
}

func (m *Manager) Query(ctx context.Context, in *protos.SqlRequest) (*protos.QueryResponse, error) {
	m.markQueryStart()
	defer m.markQueryEnd()

	d, err := m.selectDb(in)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Query SQL!!!!: key: %m, sql: `%m`\n", in.Key, in.Sql)
	rows, err := d.Query(in.Sql)
	if err != nil {
		return nil, err
	}

	return &protos.QueryResponse{Rows: rows}, nil
}

func (m *Manager) Exec(ctx context.Context, in *protos.SqlRequest) (*protos.ExecResponse, error) {
	m.markQueryStart()
	defer m.markQueryEnd()

	d, err := m.selectDb(in)
	if err != nil {
		return nil, err
	}

	fmt.Printf("EXEC SQL!!!!: key: %m, sql: `%m`\n", in.Key, in.Sql)
	result, err := d.Exec(in.Sql)
	if err != nil {
		return nil, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		// Not raise error when db doesn't support RowsAffected
		rows = ErrorNum
	}
	lId, err := result.LastInsertId()
	if err != nil {
		// Not raise error when db doesn't support LastInsertId
		lId = ErrorNum
	}
	resp := &protos.ExecResponse{
		RowsAffected:   rows,
		LastInsertedId: lId,
	}
	return resp, nil
}

func (m *Manager) markQueryStart() {
	m.stopper.WaitIfNeeded()
	m.runningQueryWg.Add(1)
}

func (m *Manager) markQueryEnd() {
	m.runningQueryWg.Done()
}

func (m *Manager) selectDb(in *protos.SqlRequest) (*db.ShardConnection, error) {
	if in.Key == "" {
		return nil, errors.New("key is empty")
	}

	sc, err := m.router.GetShardConnection(in.Key)
	if err != nil {
		return nil, err
	}

	return sc, nil
}

func (m *Manager) RegisterKey(ctx context.Context, in *protos.KeyData) (*protos.SimpleResult, error) {
	err := m.router.RegisterKey(in.Key, in.ShardName)
	if err != nil {
		return nil, err
	}

	return &protos.SimpleResult{
		Success: true,
		Message: "Success",
	}, nil
}

func (m *Manager) RemoveKey(ctx context.Context, in *protos.KeyData) (*protos.SimpleResult, error) {
	err := m.router.RemoveKey(in.Key)
	if err != nil {
		return nil, err
	}

	return &protos.SimpleResult{
		Success: true,
		Message: "Success",
	}, nil
}

func (m *Manager) MigrateShard(key, toShardName string) error {
	fromS, err := m.router.GetShardConnection(key)
	if err != nil {
		return err
	}
	toS, err := m.router.buildDb(toShardName)
	if err != nil {
		return err
	}

	a := migrator.NewApplier(key, fromS, toS)
	err = a.Run(func() bool {
		m.stopper.Stop()
		defer m.stopper.Start()

		m.runningQueryWg.Wait()
		err := m.router.RegisterKey(key, toShardName)
		if err != nil {
			return false
		}
		return true
	})
	if err != nil {
		return err
	}

	return nil
}
