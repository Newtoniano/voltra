package bot

import (
	"context"
	"fmt"
	"github.com/sleeyax/voltra/internal/config"
	"github.com/sleeyax/voltra/internal/database"
	"github.com/sleeyax/voltra/internal/database/models"
	"github.com/sleeyax/voltra/internal/market"
	"github.com/stretchr/testify/assert"
	"math"
	"sync"
	"testing"
)

type mockMarket struct {
	coinsIndex int
	coins      []market.CoinMap
	cancel     context.CancelFunc
}

// ensure mockMarket implements the Market interface
var _ market.Market = (*mockMarket)(nil)

func newMockMarket(cancel context.CancelFunc) *mockMarket {
	return &mockMarket{
		coins:  make([]market.CoinMap, 0),
		cancel: cancel,
	}
}

func (m *mockMarket) Name() string {
	return "mock market"
}

func (m *mockMarket) Buy(ctx context.Context, coin string, quantity float64) (market.Order, error) {
	panic("implement me")
}

func (m *mockMarket) Sell(ctx context.Context, coin string, quantity float64) (market.Order, error) {
	panic("implement me")
}

func (m *mockMarket) GetCoins(_ context.Context) (market.CoinMap, error) {
	if m.coinsIndex >= len(m.coins) {
		m.cancel()
		return nil, fmt.Errorf("no coins found at index %d", m.coinsIndex)
	}

	coins := m.coins[m.coinsIndex]

	m.coinsIndex++

	return coins, nil
}

func (m *mockMarket) AddCoins(coins market.CoinMap) {
	m.coins = append(m.coins, coins)
}

func (m *mockMarket) GetSymbolInfo(_ context.Context, symbol string) (market.SymbolInfo, error) {
	return market.SymbolInfo{
		Symbol:   "BTC",
		StepSize: 0.0000001,
	}, nil
}

type mockDatabase struct {
	orders map[string]models.Order
}

var _ database.Database = (*mockDatabase)(nil)

func newMockDatabase() *mockDatabase {
	return &mockDatabase{
		orders: make(map[string]models.Order),
	}
}

func (m *mockDatabase) SaveOrder(order models.Order) {
	m.orders[order.Symbol+string(order.Type)] = order
}

func (m *mockDatabase) HasOrder(orderType models.OrderType, market, symbol string) bool {
	order, ok := m.orders[symbol+string(orderType)]
	return ok && order.Type == orderType && order.Market == market
}

func (m *mockDatabase) CountOrders(orderType models.OrderType, market string) int64 {
	var count int64
	for _, order := range m.orders {
		if order.Type == orderType && order.Market == market {
			count++
		}
	}
	return count
}

func (m *mockDatabase) GetOrders(orderType models.OrderType, market string) []models.Order {
	var orders []models.Order
	for _, order := range m.orders {
		if order.Type == orderType && order.Market == market {
			orders = append(orders, order)
		}
	}
	return orders
}

func (m *mockDatabase) DeleteOrder(order models.Order) {
	delete(m.orders, order.Symbol+string(order.Type))
}

func (m *mockDatabase) SaveCache(_ models.Cache) {
	// ignore
}

func (m *mockDatabase) GetCache(_ string) (models.Cache, bool) {
	return models.Cache{}, false
}

func (m *mockDatabase) GetLastOrder(orderType models.OrderType, market, symbol string) (models.Order, bool) {
	panic("implement me")
}

func TestBot_buy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &config.Configuration{
		EnableTestMode: true,
		LoggingOptions: config.LoggingOptions{Enable: false},
		TradingOptions: config.TradingOptions{
			ChangeInPrice: 10, // 10%
			PairWith:      "USDT",
			Quantity:      10, // trade 10 USDT
		},
	}

	m := newMockMarket(cancel)
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  10_000,
		},
		"ETH": market.Coin{
			Symbol: "ETH",
			Price:  10_000,
		},
	})
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  10_500,
		},
		"ETH": market.Coin{
			Symbol: "ETH",
			Price:  9_000,
		},
	})
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  11_000,
		},
		"ETH": market.Coin{
			Symbol: "ETH",
			Price:  10_000,
		},
	})

	db := newMockDatabase()

	b := New(c, m, db)

	var wg sync.WaitGroup
	wg.Add(1)
	b.buy(ctx, &wg)

	orders := db.GetOrders(models.BuyOrder, m.Name())
	assert.Equal(t, 1, len(orders))
	assert.Equal(t, "BTC", orders[0].Symbol)
	assert.Equal(t, 0.0009091, orders[0].Volume)
}

func TestBot_sell(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &config.Configuration{
		EnableTestMode: true,
		LoggingOptions: config.LoggingOptions{Enable: false},
		TradingOptions: config.TradingOptions{
			ChangeInPrice:   0.5,
			PairWith:        "USDT",
			Quantity:        15,
			TakeProfit:      0.1,
			StopLoss:        5,
			TradingFeeTaker: 0.075,
		},
	}

	m := newMockMarket(cancel)
	m.AddCoins(market.CoinMap{
		"XTZUSDT": market.Coin{
			Symbol: "XTZUSDT",
			Price:  1.295,
		},
	})

	marketOrder := market.Order{
		Symbol: "XTZUSDT",
		Price:  1.292,
	}

	db := newMockDatabase()
	b := New(c, m, db)

	order := models.Order{
		Order:        marketOrder,
		Market:       m.Name(),
		Type:         models.BuyOrder,
		Volume:       11.6,
		HighestPrice: &marketOrder.Price,
		IsTestMode:   true,
	}

	fixedStopLoss := order.Price - (order.Price * (math.Abs(b.config.TradingOptions.StopLoss / 100)))
	fixedTakeProfit := order.Price + (order.Price * (math.Abs(b.config.TradingOptions.TakeProfit / 100)))
	order.TakeProfit = &fixedTakeProfit
	order.StopLoss = &fixedStopLoss

	db.SaveOrder(order)

	var wg sync.WaitGroup
	wg.Add(1)
	b.sell(ctx, &wg)

	assert.Equal(t, int64(1), db.CountOrders(models.SellOrder, m.Name()))
	assert.Equal(t, int64(0), db.CountOrders(models.BuyOrder, m.Name()))
}

func TestBot_sell_with_trailing_stop_loss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &config.Configuration{
		EnableTestMode: true,
		LoggingOptions: config.LoggingOptions{Enable: false},
		TradingOptions: config.TradingOptions{
			ChangeInPrice:   0.5,
			PairWith:        "USDT",
			Quantity:        10,
			TakeProfit:      20,
			StopLoss:        5,
			TradingFeeTaker: 0.075,
			TrailingStopOptions: config.TrailingStopOptions{
				Enable:           true,
				TrailingStopLoss: 1,
			},
		},
	}

	m := newMockMarket(cancel)
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  11_000,
		},
	})
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  11_050,
		},
	})
	m.AddCoins(market.CoinMap{
		"BTC": market.Coin{
			Symbol: "BTC",
			Price:  9000,
		},
	})

	marketOrder := market.Order{
		Symbol: "BTC",
		Price:  10_000,
	}
	takeProfit := marketOrder.Price + (marketOrder.Price * (math.Abs(c.TradingOptions.TakeProfit / 100)))
	stopLoss := marketOrder.Price - (marketOrder.Price * (math.Abs(c.TradingOptions.StopLoss / 100)))

	order := models.Order{
		Order:        marketOrder,
		Market:       m.Name(),
		Type:         models.BuyOrder,
		Volume:       0.000909,
		HighestPrice: &marketOrder.Price,
		TakeProfit:   &takeProfit,
		StopLoss:     &stopLoss,
		IsTestMode:   true,
	}

	db := newMockDatabase()
	db.SaveOrder(order)

	b := New(c, m, db)

	var wg sync.WaitGroup
	wg.Add(1)
	b.sell(ctx, &wg)

	assert.Equal(t, int64(0), db.CountOrders(models.BuyOrder, m.Name()))
	orders := db.GetOrders(models.SellOrder, m.Name())
	assert.Equal(t, 1, len(orders))
	assert.NotNil(t, orders[0].PriceChangePercentage)
	assert.Equal(t, float64(-10), *orders[0].PriceChangePercentage)
	assert.Equal(t, float64(9000), orders[0].Price)
}

func TestBot_convertVolume(t *testing.T) {
	c := config.Configuration{
		EnableTestMode: true,
		LoggingOptions: config.LoggingOptions{Enable: false},
	}
	b := New(&c, newMockMarket(nil), newMockDatabase())
	v, err := b.convertVolume(context.Background(), 50, market.VolatileCoin{
		Coin: market.Coin{
			Symbol: "BTC",
			Price:  100,
		},
	})
	assert.Equal(t, nil, err)
	assert.Equal(t, 0.5, v)

	v, err = b.convertVolume(context.Background(), 50, market.VolatileCoin{
		Coin: market.Coin{
			Symbol: "BTC",
			Price:  10000,
		},
	})
	assert.Equal(t, nil, err)
	assert.Equal(t, 0.005, v)

	v, err = b.convertVolume(context.Background(), 10, market.VolatileCoin{
		Coin: market.Coin{
			Symbol: "BTC",
			Price:  11_000,
		},
	})
	assert.Equal(t, nil, err)
	assert.Equal(t, 0.0009091, v)
}

func TestBot_calculateStopLoss_StandardTrailingStop(t *testing.T) {
	c := config.Configuration{
		TradingOptions: config.TradingOptions{
			TakeProfit: 5,
			StopLoss:   10,
			TrailingStopOptions: config.TrailingStopOptions{
				Enable:                     true,
				TrailingStopLoss:           10,
				TrailingStopPositive:       0.0,
				TrailingStopPositiveOffset: 3, // any value is ignored when TrailingStopPositive is 0
			},
		},
	}
	b := New(&c, newMockMarket(nil), newMockDatabase())

	buyPrice := 100.0
	stopLoss := buyPrice - (buyPrice * (math.Abs(c.TradingOptions.StopLoss / 100)))

	boughtCoin := models.Order{
		Order: market.Order{
			Price: buyPrice,
		},
		HighestPrice: &buyPrice,
		StopLoss:     &stopLoss,
	}
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 93
	b.calculateStopLoss(-7, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 100
	b.calculateStopLoss(0, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	*boughtCoin.HighestPrice = 102
	b.calculateStopLoss(2.0, &boughtCoin)
	assert.Equal(t, 91.8, *boughtCoin.StopLoss)
	boughtCoin.Price = 101
	b.calculateStopLoss(1.0, &boughtCoin)
	assert.Equal(t, 91.8, *boughtCoin.StopLoss)
	boughtCoin.Price = 95
	b.calculateStopLoss(-5, &boughtCoin)
	assert.Equal(t, 91.8, *boughtCoin.StopLoss)

}

func TestBot_calculateStopLoss_PositiveStopNoOffset(t *testing.T) {
	c := config.Configuration{
		TradingOptions: config.TradingOptions{
			TakeProfit: 5,
			StopLoss:   10,
			TrailingStopOptions: config.TrailingStopOptions{
				Enable:                     true,
				TrailingStopLoss:           10,
				TrailingStopPositive:       2.0,
				TrailingStopPositiveOffset: 0,
			},
		},
	}
	b := New(&c, newMockMarket(nil), newMockDatabase())

	buyPrice := 100.0
	stopLoss := buyPrice - (buyPrice * (math.Abs(c.TradingOptions.StopLoss / 100)))

	boughtCoin := models.Order{
		Order: market.Order{
			Price: 100,
		},
		HighestPrice: &buyPrice,
		StopLoss:     &stopLoss,
	}
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 93
	b.calculateStopLoss(-7, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 100
	b.calculateStopLoss(0, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	*boughtCoin.HighestPrice = 102
	b.calculateStopLoss(2.0, &boughtCoin)
	assert.Equal(t, 99.96, *boughtCoin.StopLoss)
	boughtCoin.Price = 101
	b.calculateStopLoss(1.0, &boughtCoin)
	assert.Equal(t, 99.96, *boughtCoin.StopLoss)
	boughtCoin.Price = 99.98
	b.calculateStopLoss(-0.02, &boughtCoin)
	assert.Equal(t, 99.96, *boughtCoin.StopLoss)
}

func TestBot_calculateStopLoss_PositiveStopWithOffset(t *testing.T) {
	c := config.Configuration{
		TradingOptions: config.TradingOptions{
			TakeProfit: 5,
			StopLoss:   10,
			TrailingStopOptions: config.TrailingStopOptions{
				Enable:                      true,
				TrailingStopLoss:            10,
				TrailingStopPositive:        2.0,
				TrailingStopPositiveOffset:  3.0,
				TrailingOnlyOffsetIsReached: false,
			},
		},
	}
	b := New(&c, newMockMarket(nil), newMockDatabase())

	buyPrice := 100.0
	stopLoss := buyPrice - (buyPrice * (math.Abs(c.TradingOptions.StopLoss / 100)))

	boughtCoin := models.Order{
		Order: market.Order{
			Price: 100,
		},
		HighestPrice: &buyPrice,
		StopLoss:     &stopLoss,
	}
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 93
	b.calculateStopLoss(-7, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	*boughtCoin.HighestPrice = 102
	b.calculateStopLoss(2.0, &boughtCoin)
	assert.Equal(t, 91.8, *boughtCoin.StopLoss)
	boughtCoin.Price = 95
	b.calculateStopLoss(-5, &boughtCoin)
	assert.Equal(t, 91.8, *boughtCoin.StopLoss)
	boughtCoin.Price = 103.5
	*boughtCoin.HighestPrice = 103.5
	b.calculateStopLoss(3.5, &boughtCoin)
	assert.Equal(t, 101.43, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	b.calculateStopLoss(2, &boughtCoin)
	assert.Equal(t, 101.43, *boughtCoin.StopLoss)
	boughtCoin.Price = 105
	*boughtCoin.HighestPrice = 105
	b.calculateStopLoss(5, &boughtCoin)
	assert.Equal(t, 102.9, *boughtCoin.StopLoss)
	boughtCoin.Price = 103
	b.calculateStopLoss(3, &boughtCoin)
	assert.Equal(t, 102.9, *boughtCoin.StopLoss)
}

func TestBot_calculateStopLoss_PositiveStopWithOffsetTrailingOnlyIfReached(t *testing.T) {
	c := config.Configuration{
		TradingOptions: config.TradingOptions{
			TakeProfit: 5,
			StopLoss:   10,
			TrailingStopOptions: config.TrailingStopOptions{
				Enable:                      true,
				TrailingStopLoss:            10,
				TrailingStopPositive:        2.0,
				TrailingStopPositiveOffset:  3.0,
				TrailingOnlyOffsetIsReached: true,
			},
		},
	}
	b := New(&c, newMockMarket(nil), newMockDatabase())

	buyPrice := 100.0
	stopLoss := buyPrice - (buyPrice * (math.Abs(c.TradingOptions.StopLoss / 100)))

	boughtCoin := models.Order{
		Order: market.Order{
			Price: 100,
		},
		HighestPrice: &buyPrice,
		StopLoss:     &stopLoss,
	}

	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 93
	b.calculateStopLoss(-7, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	*boughtCoin.HighestPrice = 102
	b.calculateStopLoss(2.0, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 95
	b.calculateStopLoss(-5, &boughtCoin)
	assert.Equal(t, 90.0, *boughtCoin.StopLoss)
	boughtCoin.Price = 103.5
	*boughtCoin.HighestPrice = 103.5
	b.calculateStopLoss(3.5, &boughtCoin)
	assert.Equal(t, 101.43, *boughtCoin.StopLoss)
	boughtCoin.Price = 102
	b.calculateStopLoss(2, &boughtCoin)
	assert.Equal(t, 101.43, *boughtCoin.StopLoss)
	boughtCoin.Price = 105
	*boughtCoin.HighestPrice = 105
	b.calculateStopLoss(5, &boughtCoin)
	assert.Equal(t, 102.9, *boughtCoin.StopLoss)
	boughtCoin.Price = 103
	b.calculateStopLoss(3, &boughtCoin)
	assert.Equal(t, 102.9, *boughtCoin.StopLoss)
}
