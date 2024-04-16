package bot

import (
	"context"
	"fmt"
	"github.com/sleeyax/voltra/internal/config"
	"github.com/sleeyax/voltra/internal/database"
	"github.com/sleeyax/voltra/internal/database/models"
	"github.com/sleeyax/voltra/internal/market"
	"github.com/sleeyax/voltra/internal/utils"
	"go.uber.org/zap"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"math"
	"sync"
	"time"
)

type Bot struct {
	market           market.Market
	db               database.Database
	volatilityWindow *VolatilityWindow
	config           *config.Configuration
	botLog           *zap.SugaredLogger
	buyLog           *zap.SugaredLogger
	sellLog          *zap.SugaredLogger
}

func New(config *config.Configuration, market market.Market, db database.Database) *Bot {
	sugaredLogger := createLogger(config.LoggingOptions).Named("bot")
	return &Bot{
		market:           market,
		db:               db,
		volatilityWindow: NewVolatilityWindow(config.TradingOptions.RecheckInterval),
		config:           config,
		botLog:           sugaredLogger,
		buyLog:           sugaredLogger.Named("buy"),
		sellLog:          sugaredLogger.Named("sell"),
	}
}

func (b *Bot) flushLogs() {
	_ = b.botLog.Sync()
	_ = b.buyLog.Sync()
	_ = b.sellLog.Sync()
}

// Start starts monitoring the market for price changes.
func (b *Bot) Start(ctx context.Context) {
	defer b.flushLogs()
	b.botLog.Info("Bot started. Press CTRL + C to quit.")

	var wg sync.WaitGroup
	wg.Add(2)

	go b.sell(ctx, &wg)
	go b.buy(ctx, &wg)

	// Wait for both buy and sell goroutines to finish.
	wg.Wait()

	b.botLog.Info("Bot stopped.")
}

func (b *Bot) buy(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	b.buyLog.Debug("Watching coins to buy.")

	if err := b.updateLatestCoins(ctx); err != nil {
		panic(fmt.Sprintf("failed to load initial latest coins: %s", err))
	}

	for {
		select {
		case <-ctx.Done():
			b.buyLog.Debug("Bot stopped buying coins.")
			return
		default:
			// Wait until the next recheck interval.
			lastRecord := b.volatilityWindow.GetLatestRecord()
			delta := utils.CalculateTimeDuration(b.config.TradingOptions.TimeDifference, b.config.TradingOptions.RecheckInterval)
			if time.Since(lastRecord.time) < delta {
				interval := delta - time.Since(lastRecord.time)
				b.buyLog.Debugf("Waiting %s.", interval.Round(time.Second))
				time.Sleep(interval)
			}

			// Fetch the latest coins again after the waiting period.
			if err := b.updateLatestCoins(ctx); err != nil {
				b.buyLog.Errorf("Failed to update latest coins: %s.", err)
				continue
			}

			// Identify volatile coins in the current time window and trade them if any are found.
			volatileCoins := b.volatilityWindow.IdentifyVolatileCoins(b.config.TradingOptions.ChangeInPrice)
			b.buyLog.Infof("Found %d volatile coins.", len(volatileCoins))
			for _, volatileCoin := range volatileCoins {
				b.buyLog.Infof("Coin %s has gained %.2f%% within the last %d minutes.", volatileCoin.Symbol, volatileCoin.Percentage, b.config.TradingOptions.TimeDifference)

				// Skip if the coin has already been bought.
				if b.db.HasOrder(models.BuyOrder, b.market.Name(), volatileCoin.Symbol) {
					b.buyLog.Warnf("Already bought %s. Skipping.", volatileCoin.Symbol)
					continue
				}

				// Skip if the max amount of buy orders has been reached.
				if maxBuyOrders := int64(b.config.TradingOptions.MaxCoins); maxBuyOrders != 0 && b.db.CountOrders(models.BuyOrder, b.market.Name()) >= maxBuyOrders {
					b.buyLog.Warnf("Max amount of buy orders reached. Skipping.")
					continue
				}

				// Skip if the coin has been sold very recently (within the cool-off period)
				if coolOffDelay := time.Duration(b.config.TradingOptions.CoolOffDelay) * time.Minute; coolOffDelay != 0 {
					lastOrder, ok := b.db.GetLastOrder(models.SellOrder, b.market.Name(), volatileCoin.Symbol)
					if ok && time.Since(lastOrder.CreatedAt) < coolOffDelay {
						b.buyLog.Warnf("Already bought %s within the configured cool-off period of %s. Skipping.", volatileCoin.Symbol, coolOffDelay)
						continue
					}
				}

				// Determine the correct volume to buy based on the configured quantity.
				volume, err := b.convertVolume(ctx, b.config.TradingOptions.Quantity, volatileCoin)
				if err != nil {
					b.buyLog.Errorf("Failed to convert volume. Skipping the trade: %s", err)
					continue
				}

				b.buyLog.Infow(fmt.Sprintf("Buying %g %s of %s.", volume, b.config.TradingOptions.PairWith, volatileCoin.Symbol),
					"volume", volume,
					"pair_with", b.config.TradingOptions.PairWith,
					"symbol", volatileCoin.Symbol,
					"price", volatileCoin.Price,
					"percentage", volatileCoin.Percentage,
					"testMode", b.config.EnableTestMode,
				)

				order := models.Order{
					Market: b.market.Name(),
					Type:   models.BuyOrder,
					Volume: volume,
				}

				// Pretend to buy the coin and save the order if test mode is enabled.
				if b.config.EnableTestMode {
					order.Order = market.Order{
						OrderID:         0,
						Symbol:          volatileCoin.Symbol,
						Price:           volatileCoin.Price,
						TransactionTime: time.Now(),
					}
					order.IsTestMode = true
				} else {
					// Otherwise, buy the coin and save the real order.
					buyOrder, err := b.market.Buy(ctx, volatileCoin.Symbol, volume)
					if err != nil {
						b.buyLog.Errorf("Failed to buy %s: %s.", volatileCoin.Symbol, err)
						continue
					}

					order.Order = buyOrder
				}

				// use the real order execution price to calculate the fixed stop loss and take profit
				fixedStopLoss := order.Price - (order.Price * (math.Abs(b.config.TradingOptions.StopLoss / 100)))
				order.StopLoss = &fixedStopLoss
				fixedTakeProfit := order.Price + (order.Price * (math.Abs(b.config.TradingOptions.TakeProfit / 100)))
				order.TakeProfit = &fixedTakeProfit

				order.HighestPrice = &order.Price

				b.db.SaveOrder(order)
			}
		}
	}
}

func (b *Bot) sell(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	b.sellLog.Debug("Watching coins to sell.")

	for {
		select {
		case <-ctx.Done():
			b.sellLog.Debug("Bot stopped selling coins.")
			return
		default:
			coins, err := b.market.GetCoins(ctx)
			if err != nil {
				b.sellLog.Errorf("Failed to fetch coins: %s.", err)
				continue
			}

			orders := b.db.GetOrders(models.BuyOrder, b.market.Name())
			for _, boughtCoin := range orders {
				buyPrice := boughtCoin.Price
				currentPrice := coins[boughtCoin.Symbol].Price
				if currentPrice > *boughtCoin.HighestPrice {
					boughtCoin.HighestPrice = &currentPrice
				}
				priceChangePercentage := (currentPrice - buyPrice) / buyPrice * 100

				sellFee := currentPrice * (b.config.TradingOptions.TradingFeeTaker / 100)
				buyFee := buyPrice * (b.config.TradingOptions.TradingFeeTaker / 100)
				fees := buyFee + sellFee

				if b.config.TradingOptions.TrailingStopOptions.Enable {
					b.calculateStopLoss(priceChangePercentage, &boughtCoin)
					b.sellLog.Debugw(
						"priceChangePercentage", priceChangePercentage,
						"trailingStopLossPct", b.config.TradingOptions.TrailingStopOptions.TrailingStopLoss,
						"trailingStopPositivePct", b.config.TradingOptions.TrailingStopOptions.TrailingStopPositive,
						"trailingStopPositiveOffset", b.config.TradingOptions.TrailingStopOptions.TrailingStopPositiveOffset,
						"stopLoss", *boughtCoin.StopLoss,
						"takeProfit", *boughtCoin.TakeProfit,
					)
					b.db.SaveOrder(boughtCoin)
				}

				// If the price of the coin is below the stop loss or above take profit then sell it.
				if currentPrice <= *boughtCoin.StopLoss || currentPrice >= *boughtCoin.TakeProfit {
					estimatedProfitLoss := (currentPrice-buyPrice)*boughtCoin.Volume - fees
					estimatedProfitLossPercentage := estimatedProfitLoss / (buyPrice * boughtCoin.Volume) * 100
					msg := fmt.Sprintf(
						"Selling %g %s. Estimated %s: $%.2f %.2f%%",
						boughtCoin.Volume,
						boughtCoin.Symbol,
						b.getProfitOrLossText(priceChangePercentage),
						estimatedProfitLoss,
						estimatedProfitLossPercentage,
					)

					b.sellLog.Infow(
						msg,
						"buyPrice", buyPrice,
						"currentPrice", currentPrice,
						"priceChangePercentage", priceChangePercentage,
						"tradingFeeMaker", b.config.TradingOptions.TradingFeeMaker,
						"tradingFeeTaker", b.config.TradingOptions.TradingFeeTaker,
						"fees", fees,
						"quantity", b.config.TradingOptions.Quantity,
						"testMode", b.config.EnableTestMode,
					)

					order := models.Order{
						Market:                b.market.Name(),
						Type:                  models.SellOrder,
						Volume:                boughtCoin.Volume,
						PriceChangePercentage: &priceChangePercentage,
						EstimatedProfitLoss:   &estimatedProfitLoss,
					}

					if b.config.EnableTestMode {
						order.Order = market.Order{
							OrderID:         0,
							Symbol:          boughtCoin.Symbol,
							TransactionTime: time.Now(),
							Price:           currentPrice,
						}
						order.IsTestMode = true
					} else {
						sellOrder, err := b.market.Sell(ctx, boughtCoin.Symbol, boughtCoin.Volume)
						if err != nil {
							b.sellLog.Errorf("Failed to sell %s: %s.", boughtCoin.Symbol, err)
							continue
						}

						order.Order = sellOrder
					}

					// Determine actual profit/loss of the filled order.
					sellPrice := order.Price
					sellFee = sellPrice * (b.config.TradingOptions.TradingFeeTaker / 100)
					priceChangePercentage = (sellPrice - buyPrice) / buyPrice * 100
					fees = buyFee + sellFee
					profitLoss := (sellPrice-buyPrice)*order.Volume - fees
					profitLossPercentage := profitLoss / (buyPrice * order.Volume) * 100
					order.RealProfitLoss = &profitLoss
					msg = fmt.Sprintf(
						"Sold %g %s. %s: $%.2f %.2f%%",
						boughtCoin.Volume,
						boughtCoin.Symbol,
						cases.Title(language.English).String(b.getProfitOrLossText(profitLossPercentage)),
						profitLoss,
						profitLossPercentage,
					)

					b.sellLog.Infow(
						msg,
						"buyPrice", buyPrice,
						"currentPrice", currentPrice,
						"sellPrice", sellPrice,
						"priceChangePercentage", priceChangePercentage,
						"tradingFeeMaker", b.config.TradingOptions.TradingFeeMaker,
						"tradingFeeTaker", b.config.TradingOptions.TradingFeeTaker,
						"fees", fees,
						"quantity", b.config.TradingOptions.Quantity,
						"testMode", b.config.EnableTestMode,
					)

					if b.config.TradingOptions.EnableDynamicQuantity {
						b.config.TradingOptions.Quantity += profitLoss / float64(b.config.TradingOptions.MaxCoins)
					}

					b.db.SaveOrder(order)
					b.db.DeleteOrder(boughtCoin)

					continue
				}

				b.sellLog.Infow(
					fmt.Sprintf("Price of %s is %.2f%% away from the buy price. Hodl.", boughtCoin.Symbol, priceChangePercentage),
					"symbol", boughtCoin.Symbol,
					"buyPrice", buyPrice,
					"currentPrice", currentPrice,
					"takeProfit", *boughtCoin.TakeProfit,
					"stopLoss", *boughtCoin.StopLoss,
				)
			}

			time.Sleep(time.Second * time.Duration(b.config.TradingOptions.SellTimeout))
		}
	}
}

func (b *Bot) calculateStopLoss(priceChangePercentage float64, boughtCoin *models.Order) {
	previousStopLoss := *boughtCoin.StopLoss
	var newStopLoss float64
	if b.config.TradingOptions.TrailingStopOptions.TrailingStopPositive != 0.0 {
		// if the position is in profit, the trailing stop loss will be activated and will replace the default non-trailing stop loss.
		if priceChangePercentage > math.Abs(b.config.TradingOptions.TrailingStopOptions.TrailingStopPositiveOffset) {
			newStopLoss = *boughtCoin.HighestPrice - (*boughtCoin.HighestPrice * (b.config.TradingOptions.TrailingStopOptions.TrailingStopPositive / 100))
		} else if b.config.TradingOptions.TrailingStopOptions.TrailingOnlyOffsetIsReached {
			newStopLoss = previousStopLoss
		} else {
			newStopLoss = math.Max(boughtCoin.Price-(boughtCoin.Price*(math.Abs(b.config.TradingOptions.TrailingStopOptions.TrailingStopLoss/100))), *boughtCoin.StopLoss)
		}
	} else {
		// default trailing stop loss behavior, use static stop loss as hard stop but move the stop loss upwards if the position goes into profit.
		newStopLoss = math.Max(*boughtCoin.StopLoss, *boughtCoin.HighestPrice-(*boughtCoin.HighestPrice*(math.Abs(b.config.TradingOptions.TrailingStopOptions.TrailingStopLoss/100))))
	}
	if newStopLoss != 0.0 {
		boughtCoin.StopLoss = &newStopLoss
	}
}

func (b *Bot) getProfitOrLossText(priceChangePercentage float64) string {
	var profitOrLossText string
	if priceChangePercentage >= 0 {
		profitOrLossText = "profit"
	} else {
		profitOrLossText = "loss"
	}
	return profitOrLossText
}

// updateLatestCoins fetches the latest coins from the market and appends them to the volatilityWindow.
func (b *Bot) updateLatestCoins(ctx context.Context) error {
	b.botLog.Debug("Fetching latest coins.")

	coins, err := b.market.GetCoins(ctx)
	if err != nil {
		return err
	}

	b.volatilityWindow.AddRecord(coins)

	return nil
}

// convertVolume converts the volume given in the configured quantity from base currency (USDT) to each coin's volume.
func (b *Bot) convertVolume(ctx context.Context, quantity float64, volatileCoin market.VolatileCoin) (float64, error) {
	var stepSize float64

	// Get the step size of the coin from the local cache if it exists or from Binance if it doesn't (yet).
	// The step size never changes, so it's safe to cache it forever.
	// This approach avoids an additional API request to Binance per trade.
	cache, ok := b.db.GetCache(volatileCoin.Symbol)
	if ok {
		stepSize = cache.StepSize
	} else {
		info, err := b.market.GetSymbolInfo(ctx, volatileCoin.Symbol)
		if err != nil {
			return 0, err
		}

		stepSize = info.StepSize

		b.db.SaveCache(models.Cache{Symbol: volatileCoin.Symbol, StepSize: stepSize})
	}

	volume := quantity / volatileCoin.Price

	// Round the volume to the step size of the coin.
	if stepSize != 0 {
		volume = utils.RoundStepSize(volume, stepSize)
	}

	return volume, nil
}
