//go:build integration

package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *RedeemCodeRepoSuite) TestListWithFilters_SortByValueAsc() {
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "VALUE-20", Type: service.RedeemTypeBalance, Value: 20, Status: service.StatusUnused}))
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "VALUE-10", Type: service.RedeemTypeBalance, Value: 10, Status: service.StatusUnused}))

	codes, _, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{
		Page:      1,
		PageSize:  10,
		SortBy:    "value",
		SortOrder: "asc",
	}, "", "", "", "", nil)
	s.Require().NoError(err)
	s.Require().Len(codes, 2)
	s.Require().Equal("VALUE-10", codes[0].Code)
	s.Require().Equal("VALUE-20", codes[1].Code)
}

func (s *RedeemCodeRepoSuite) TestListWithFilters_ValueExactMatch() {
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "VALF-10", Type: service.RedeemTypeBalance, Value: 10, Status: service.StatusUnused}))
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "VALF-20", Type: service.RedeemTypeBalance, Value: 20, Status: service.StatusUnused}))

	value := 10.0
	codes, page, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, service.RedeemTypeBalance, "", "", "", &value)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), page.Total)
	s.Require().Len(codes, 1)
	s.Require().Equal("VALF-10", codes[0].Code)
}

func (s *RedeemCodeRepoSuite) TestListDistinctValues() {
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "DIST-10", Type: service.RedeemTypeBalance, Value: 10, Status: service.StatusUnused}))
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "DIST-10-B", Type: service.RedeemTypeBalance, Value: 10, Status: service.StatusUsed}))
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "DIST-20", Type: service.RedeemTypeBalance, Value: 20, Status: service.StatusUnused}))
	s.Require().NoError(s.repo.Create(s.ctx, &service.RedeemCode{Code: "DIST-5", Type: service.RedeemTypeConcurrency, Value: 5, Status: service.StatusUnused}))

	all, err := s.repo.ListDistinctValues(s.ctx, "")
	s.Require().NoError(err)
	s.Require().Equal([]float64{5, 10, 20}, all)

	balance, err := s.repo.ListDistinctValues(s.ctx, service.RedeemTypeBalance)
	s.Require().NoError(err)
	s.Require().Equal([]float64{10, 20}, balance)
}
