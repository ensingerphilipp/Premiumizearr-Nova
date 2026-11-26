const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const { EsbuildPlugin } = require("esbuild-loader");
const path = require('path');
const CopyWebpackPlugin = require('copy-webpack-plugin');
const { optimizeImports } = require("carbon-preprocess-svelte");

const mode = process.env.NODE_ENV || 'development';
const prod = mode === 'production';
module.exports = {
	entry: {
		'bundle': ['./src/main.js']
	},
	resolve: {
		extensions: ['.mjs', '.js', '.svelte'],
		conditionNames: ['svelte', 'browser', '...'],
		mainFields: ['svelte', 'browser', '...']
	},
	output: {
		path: path.join(__dirname, '/dist'),
		filename: '[name].js',
		chunkFilename: '[name].[id].js'
	},
	module: {
		rules: [
			{
				test: /\.svelte$/,
				use: {
					loader: "svelte-loader",
					options: {
						preprocess: [optimizeImports()],
						hotReload: !prod,
						compilerOptions: { dev: !prod },
					},
				},
			},
			{
				test: /\.css$/,
				use: [MiniCssExtractPlugin.loader, "css-loader"],
			},
			{
				test: /node_modules\/svelte\/.*\.mjs$/,
				resolve: { fullySpecified: false },
			},
		]
	},
	mode,
	plugins: [
		new MiniCssExtractPlugin({
			filename: '[name].css'
		}),
		new CopyWebpackPlugin({
			patterns: [
				{ from: 'public' }
			]
		})
	],
	devtool: 'source-map',
	devServer: {
		hot: true,
		proxy: [
			{ context: ['/api'], target: 'http://127.0.0.1:8182' }
		]
	},
	optimization: {
		minimizer: [new EsbuildPlugin({ target: "es2020" })],
	},
};
