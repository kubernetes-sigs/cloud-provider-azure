package resources

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/data/table"
	"github.com/Azure/azure-kusto-go/kusto/data/types"
	"github.com/Azure/azure-kusto-go/kusto/data/value"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
)

type FakeMgmt struct {
	mock       *kusto.MockRows
	DBEqual    string
	QueryEqual string
	mgmtErr    bool
}

func FakeResources(rows []value.Values, setErr bool) *FakeMgmt {
	cols := table.Columns{
		{
			Name: "ResourceTypeName",
			Type: types.String,
		},
		{
			Name: "StorageRoot",
			Type: types.String,
		},
	}

	fm := NewFakeMgmt(cols, rows, setErr)
	return fm
}

func NewFakeMgmt(columns table.Columns, rows []value.Values, setErr bool) *FakeMgmt {
	mock, err := kusto.NewMockRows(columns)
	if err != nil {
		panic(err)
	}

	for _, row := range rows {
		_ = mock.Row(row)
	}

	if setErr {
		_ = mock.Error(errors.New("some error"))
	}

	return &FakeMgmt{
		mock: mock,
	}
}

func (f *FakeMgmt) SetDBEquals(s string) *FakeMgmt {
	f.DBEqual = s
	return f
}

func (f *FakeMgmt) SetQueryEquals(s string) *FakeMgmt {
	f.DBEqual = s
	return f
}

func (f *FakeMgmt) SetMgmtErr() *FakeMgmt {
	f.mgmtErr = true
	return f
}

func (f *FakeMgmt) Mgmt(_ context.Context, db string, query kusto.Stmt, _ ...kusto.MgmtOption) (*kusto.RowIterator, error) {
	if f.DBEqual != "" {
		if db != f.DBEqual {
			panic(fmt.Sprintf("expected db to be %q, was %q", f.DBEqual, db))
		}
	}
	if f.QueryEqual != "" {
		if query.String() != f.QueryEqual {
			panic(fmt.Sprintf("expected query to be %q, was %q", f.QueryEqual, db))
		}
	}
	if f.mgmtErr {
		return nil, fmt.Errorf("some mgmt error")
	}
	iter := &kusto.RowIterator{}
	if err := iter.Mock(f.mock); err != nil {
		panic(err)
	}
	return iter, nil
}

func SuccessfulFakeResources() *FakeMgmt {
	return FakeResources(
		[]value.Values{
			{
				value.String{
					Valid: true,
					Value: "TempStorage",
				},
				value.String{
					Valid: true,
					Value: "https://account.blob.core.windows.net/storageroot0",
				},
			},
			{
				value.String{
					Valid: true,
					Value: "SecuredReadyForAggregationQueue",
				},
				value.String{
					Valid: true,
					Value: "https://account.blob.core.windows.net/storageroot1",
				},
			},
		},
		false,
	)
}

type FsMock struct {
	OnLocal  func(ctx context.Context, from string, props properties.All) error
	OnReader func(ctx context.Context, reader io.Reader, props properties.All) (string, error)
	OnBlob   func(ctx context.Context, from string, fileSize int64, props properties.All) error
}

func (f FsMock) Close() error {
	return nil
}

func (f FsMock) Local(ctx context.Context, from string, props properties.All) error {
	if f.OnLocal != nil {
		return f.OnLocal(ctx, from, props)
	}
	return nil
}

func (f FsMock) Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error) {
	if f.OnReader != nil {
		return f.OnReader(ctx, reader, props)
	}
	return "", nil
}

func (f FsMock) Blob(ctx context.Context, from string, fileSize int64, props properties.All) error {
	if f.OnBlob != nil {
		return f.OnBlob(ctx, from, fileSize, props)
	}
	return nil
}
